package push

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
	"github.com/silopost/silo-server/internal/worker"
)

// JobReceipts is the general job that reads the delivery receipts of sent
// push messages (ReceiptDelay after the send). Payload: receiptsPayload.
const JobReceipts = "push.receipts"

// receiptsPayload carries the ticket ids of one push.notify run with their
// devices (no table: receipts are only kept by Expo for 24 hours).
type receiptsPayload struct {
	Tickets []ticketRef `json:"tickets"`
}

type ticketRef struct {
	ID       string `json:"id"`
	DeviceID int64  `json:"deviceId"`
}

// RegisterJobs binds push.notify and push.receipts. Both are general jobs
// (no account id), so they never wait behind the IMAP operations of an
// account. With push switched off they do nothing.
func (s *Service) RegisterJobs(w *worker.Worker) {
	w.Register(mail.JobPushNotify, s.runNotify)
	w.Register(JobReceipts, s.runReceipts)
}

// MaxNotifiedMessages is the largest burst whose messages the push names
// (mailData.MessageIDs), so that the app shows one notification for each.
const MaxNotifiedMessages = 5

// runNotify is push.notify: one message per device of the user that wants
// the account, about the messages of the job that are still unread in INBOX
// and not from the user. Tickets: DeviceNotRegistered deletes the device,
// other errors are stored in last_error; ok tickets are checked by
// push.receipts. A failed request (network, 429, 5xx) retries the job with
// the queue's backoff; the messages are checked again then.
func (s *Service) runNotify(ctx context.Context, job worker.Job) error {
	if !s.Enabled() {
		return nil
	}
	var p mail.PushNotifyPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return worker.Permanent(err)
	}
	q := sqlcgen.New(s.o.Pool)
	msgs, err := q.ListPushMessages(ctx, sqlcgen.ListPushMessagesParams{AccountID: p.AccountID, Ids: p.MessageIDs})
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		return nil
	}
	targets, err := q.ListPushTargets(ctx, p.AccountID)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}
	data := mailData{V: 1, Type: "mail", AccountID: apiID("acc_", p.AccountID), ThreadID: apiID("t_", msgs[0].ThreadID), Count: len(msgs)}
	if len(msgs) <= MaxNotifiedMessages {
		data.MessageIDs = make([]string, len(msgs))
		for i, m := range msgs {
			data.MessageIDs[i] = apiID("m_", m.ID)
		}
	}
	out := make([]expoMessage, len(targets))
	devices := make([]int64, len(targets))
	for i, t := range targets {
		devices[i] = t.ID
		out[i] = expoMessage{To: t.PushToken, Data: data, Priority: "high"}
		// A data-only message has nothing but data and priority: Expo turns a
		// message with a channelId (or a title) into an FCM notification
		// message, which the system shows by itself instead of waking the app.
		if !t.DataOnly {
			out[i].ChannelID = "mail"
			out[i].Title, out[i].Sound = Title(t.Language, len(msgs)), "default"
			out[i].Tag, out[i].CollapseID = "mail-"+data.AccountID, "mail-"+data.AccountID
		}
	}
	var tickets []ticketRef
	// A retried job sends the chunks that went out before the failure again;
	// a user has far fewer than 100 devices, so there is one chunk.
	for start := 0; start < len(out); start += expoMaxMessages {
		end := min(start+expoMaxMessages, len(out))
		got, err := s.sendChunk(ctx, out[start:end], devices[start:end])
		if err != nil {
			return err
		}
		tickets = append(tickets, got...)
	}
	if len(tickets) == 0 {
		return nil
	}
	if _, err := worker.Enqueue(ctx, s.o.Pool, worker.NewJob{
		Kind: JobReceipts, Payload: receiptsPayload{Tickets: tickets}, RunAt: time.Now().Add(s.o.ReceiptDelay),
	}); err != nil {
		s.log.Warn("queue push receipts", "err", err)
	}
	return nil
}

// sendChunk sends messages to devices (same order) and handles the tickets.
// A request refused with a 4xx other than 429 (e.g. tokens of different
// Expo projects together) is sent again one message at a time; a single
// message refused so records the error on its device.
func (s *Service) sendChunk(ctx context.Context, msgs []expoMessage, devices []int64) ([]ticketRef, error) {
	res, err := s.expo.send(ctx, msgs)
	if err != nil {
		re, ok := asRequestError(err)
		if !ok || re.Retry {
			return nil, err
		}
		if len(msgs) > 1 {
			var out []ticketRef
			for i := range msgs {
				got, err := s.sendChunk(ctx, msgs[i:i+1], devices[i:i+1])
				if err != nil {
					return nil, err
				}
				out = append(out, got...)
			}
			return out, nil
		}
		s.log.Warn("push request refused", "device", devices[0], "status", re.Status, "err", re.Msg)
		return nil, s.deviceFailed(ctx, devices[0], re.Msg)
	}
	var out []ticketRef
	q := sqlcgen.New(s.o.Pool)
	for i, t := range res {
		if err := s.handleResult(ctx, devices[i], t); err != nil {
			return nil, err
		}
		if t.Status == "ok" {
			if err := q.PushDeviceSent(ctx, devices[i]); err != nil {
				return nil, err
			}
			if t.ID != "" {
				out = append(out, ticketRef{ID: t.ID, DeviceID: devices[i]})
			}
		}
	}
	return out, nil
}

// handleResult applies an error ticket or receipt to its device.
func (s *Service) handleResult(ctx context.Context, device int64, r expoResult) error {
	if r.Status != "error" {
		return nil
	}
	if r.Details.Error == expoErrDeviceNotRegistered {
		s.log.Info("push device not registered any more; removed", "device", device)
		return sqlcgen.New(s.o.Pool).DeletePushDeviceByID(ctx, device)
	}
	s.log.Warn("push to device failed", "device", device, "err", r.failure())
	return s.deviceFailed(ctx, device, r.failure())
}

func (s *Service) deviceFailed(ctx context.Context, device int64, msg string) error {
	return sqlcgen.New(s.o.Pool).PushDeviceFailed(ctx, sqlcgen.PushDeviceFailedParams{
		ID: device, LastError: pgtype.Text{String: msg, Valid: true},
	})
}

// runReceipts is push.receipts: DeviceNotRegistered deletes the device,
// other errors go to last_error. Receipts not ready are skipped (Expo keeps
// them for 24 hours; one check is enough for the device state).
func (s *Service) runReceipts(ctx context.Context, job worker.Job) error {
	if !s.Enabled() {
		return nil
	}
	var p receiptsPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return worker.Permanent(err)
	}
	for start := 0; start < len(p.Tickets); start += expoMaxReceipts {
		chunk := p.Tickets[start:min(start+expoMaxReceipts, len(p.Tickets))]
		ids := make([]string, len(chunk))
		for i, t := range chunk {
			ids[i] = t.ID
		}
		got, err := s.expo.receipts(ctx, ids)
		if err != nil {
			if re, ok := asRequestError(err); ok && !re.Retry {
				s.log.Warn("push receipts refused", "status", re.Status, "err", re.Msg)
				continue
			}
			return err
		}
		for _, t := range chunk {
			if r, ok := got[t.ID]; ok {
				if err := s.handleResult(ctx, t.DeviceID, r); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Title is the visible title of a new-mail notification in the user's
// language (users.settings.language; English when unset or unknown). It
// never names the sender, the subject or the account.
func Title(lang string, n int) string {
	switch lang {
	case "pl":
		switch {
		case n <= 1:
			return "Nowa wiadomość"
		case n%10 >= 2 && n%10 <= 4 && (n%100 < 12 || n%100 > 14):
			return fmt.Sprintf("%d nowe wiadomości", n)
		default:
			return fmt.Sprintf("%d nowych wiadomości", n)
		}
	default:
		if n <= 1 {
			return "New message"
		}
		return fmt.Sprintf("%d new messages", n)
	}
}

// apiID is the API form of a database id (prefix + base36), as the REST
// API and the SSE events give it.
func apiID(prefix string, id int64) string { return prefix + strconv.FormatInt(id, 36) }
