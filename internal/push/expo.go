package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Limits of the Expo Push Service per request.
const (
	expoMaxMessages = 100
	expoMaxReceipts = 1000
)

// expoErrDeviceNotRegistered is the ticket/receipt error after which a token
// must not be used again.
const expoErrDeviceNotRegistered = "DeviceNotRegistered"

// expoMessage is one push message (https://docs.expo.dev/push-notifications/sending-notifications/).
// It carries no message content: the title is generic, data holds ids only.
type expoMessage struct {
	To        string   `json:"to"`
	Title     string   `json:"title"`
	Data      mailData `json:"data"`
	ChannelID string   `json:"channelId"` // Android notification channel
	Priority  string   `json:"priority"`
	Sound     string   `json:"sound"` // iOS
	// Tag (Android) and CollapseID (iOS) replace the notification shown
	// for the same account.
	Tag        string `json:"tag"`
	CollapseID string `json:"collapseId"`
}

// mailData is the data of a new-mail notification; ids are API ids.
type mailData struct {
	V         int    `json:"v"`
	Type      string `json:"type"`
	AccountID string `json:"accountId"`
	ThreadID  string `json:"threadId"`
	Count     int    `json:"count"`
}

// expoResult is a push ticket or receipt.
type expoResult struct {
	Status  string `json:"status"` // ok | error
	ID      string `json:"id"`     // tickets only: the receipt id
	Message string `json:"message"`
	Details struct {
		Error string `json:"error"`
	} `json:"details"`
}

// failure describes a ticket or receipt error for push_devices.last_error.
func (r expoResult) failure() string {
	msg := r.Details.Error
	if r.Message != "" {
		if msg != "" {
			msg += ": "
		}
		msg += r.Message
	}
	if msg == "" {
		msg = "push failed"
	}
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return msg
}

// requestError is a failed request to the Expo Push Service. Retry is true
// for network errors, 429 and 5xx (the job is retried with backoff); other
// 4xx are a problem of the request that a retry does not fix.
type requestError struct {
	Status int
	Retry  bool
	Msg    string
}

func (e *requestError) Error() string {
	if e.Status == 0 {
		return "expo: " + e.Msg
	}
	return fmt.Sprintf("expo: HTTP %d: %s", e.Status, e.Msg)
}

type expoClient struct {
	http                 *http.Client
	sendURL, receiptsURL string
}

// send posts up to expoMaxMessages messages and returns their tickets in
// order.
func (c *expoClient) send(ctx context.Context, msgs []expoMessage) ([]expoResult, error) {
	var out struct {
		Data []expoResult `json:"data"`
	}
	if err := c.post(ctx, c.sendURL, msgs, &out); err != nil {
		return nil, err
	}
	if len(out.Data) != len(msgs) {
		return nil, &requestError{Status: http.StatusOK, Retry: true, Msg: fmt.Sprintf("%d tickets for %d messages", len(out.Data), len(msgs))}
	}
	return out.Data, nil
}

// receipts reads the receipts of up to expoMaxReceipts tickets; receipts not
// ready yet are missing from the map.
func (c *expoClient) receipts(ctx context.Context, ids []string) (map[string]expoResult, error) {
	var out struct {
		Data map[string]expoResult `json:"data"`
	}
	if err := c.post(ctx, c.receiptsURL, map[string][]string{"ids": ids}, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

func (c *expoClient) post(ctx context.Context, url string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return &requestError{Msg: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return &requestError{Retry: true, Msg: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return &requestError{Status: resp.StatusCode, Retry: true, Msg: err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return &requestError{
			Status: resp.StatusCode,
			Retry:  resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
			Msg:    requestErrors(data),
		}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &requestError{Status: resp.StatusCode, Retry: true, Msg: "invalid response: " + err.Error()}
	}
	return nil
}

// requestErrors summarizes the "errors" of a failed request.
func requestErrors(body []byte) string {
	var e struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &e) != nil || len(e.Errors) == 0 {
		return "request failed"
	}
	parts := make([]string, 0, len(e.Errors))
	for _, x := range e.Errors {
		parts = append(parts, strings.TrimSpace(x.Code+": "+x.Message))
	}
	msg := strings.Join(parts, "; ")
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return msg
}

func asRequestError(err error) (*requestError, bool) {
	var re *requestError
	return re, errors.As(err, &re)
}
