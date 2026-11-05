package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/silopost/silo-server/internal/accounts"
)

// Stable machine-readable error codes (plan 5.3). The frontend branches on these.
const (
	CodeValidation         = "validation_failed"
	CodeUnauthenticated    = "unauthenticated"
	CodeInvalidCredentials = "invalid_credentials"
	CodeCSRF               = "csrf_failed"
	CodeForbidden          = "forbidden"
	CodeNotFound           = "not_found"
	CodeConflict           = "conflict"
	CodeRateLimited        = "rate_limited"
	CodeUpstreamIMAP       = "upstream_imap_error"
	CodeAccountTestFailed  = "account_test_failed"
	CodeHostNotAllowed     = "host_not_allowed"
	CodeCustomServers      = "custom_servers_disabled"
	CodeAccountLimit       = "account_limit_reached"
	CodeAccountExists      = "account_exists"
	CodeInternal           = "internal_error"
)

const errorTypeBase = "https://silo.dev/errors/"

// Problem is the RFC 9457 error body (application/problem+json) with Silo's
// stable `code`. It replaces huma's default error model.
type Problem struct {
	Type   string               `json:"type,omitempty" format:"uri" doc:"URI identifying the error type."`
	Title  string               `json:"title,omitempty" doc:"Short summary of the HTTP status."`
	Status int                  `json:"status,omitempty" doc:"HTTP status code."`
	Detail string               `json:"detail,omitempty" doc:"Explanation specific to this occurrence."`
	Code   string               `json:"code,omitempty" doc:"Stable machine-readable error code."`
	Errors []*huma.ErrorDetail  `json:"errors,omitempty" doc:"Individual validation errors."`
	Test   *accounts.TestResult `json:"test,omitempty" doc:"Per-step result, present for account_test_failed."`
	header http.Header          `json:"-"`
}

// Error implements error.
func (p *Problem) Error() string { return p.Detail }

// GetStatus implements huma.StatusError.
func (p *Problem) GetStatus() int { return p.Status }

// GetHeaders implements huma.HeadersError.
func (p *Problem) GetHeaders() http.Header { return p.header }

// ContentType makes huma answer with application/problem+json.
func (p *Problem) ContentType(ct string) string {
	if ct == "application/json" {
		return "application/problem+json"
	}
	return ct
}

func (p *Problem) with(code string) *Problem {
	p.Code = code
	p.Type = errorTypeBase + strings.ReplaceAll(code, "_", "-")
	return p
}

func init() {
	// huma builds its own errors (validation, bad JSON, unexpected errors)
	// through NewError; route them to Problem so every response has a code.
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		p := &Problem{Status: status, Title: http.StatusText(status), Detail: msg}
		for _, e := range errs {
			if d, ok := e.(huma.ErrorDetailer); ok {
				p.Errors = append(p.Errors, d.ErrorDetail())
			} else if e != nil {
				p.Errors = append(p.Errors, &huma.ErrorDetail{Message: e.Error()})
			}
		}
		code := CodeInternal
		switch {
		case status == http.StatusUnprocessableEntity && msg == "validation failed":
			// The plan reserves 400 for request validation (422 is for domain failures).
			p.Status, p.Title, code = http.StatusBadRequest, http.StatusText(http.StatusBadRequest), CodeValidation
		case status == http.StatusBadRequest:
			code = CodeValidation
		case status == http.StatusUnauthorized:
			code = CodeUnauthenticated
		case status == http.StatusForbidden:
			code = CodeForbidden
		case status == http.StatusNotFound:
			code = CodeNotFound
		case status == http.StatusConflict:
			code = CodeConflict
		case status == http.StatusTooManyRequests:
			code = CodeRateLimited
		case status < 500:
			code = CodeValidation
		}
		return p.with(code)
	}
}

func problem(status int, code, detail string) *Problem {
	return (&Problem{Status: status, Title: http.StatusText(status), Detail: detail}).with(code)
}

func errUnauthenticated() error {
	return problem(http.StatusUnauthorized, CodeUnauthenticated, "A valid session is required.")
}

func errInvalidCredentials() error {
	return problem(http.StatusUnauthorized, CodeInvalidCredentials, "Invalid email or password.")
}

func errNotFound() error { return problem(http.StatusNotFound, CodeNotFound, "Not found.") }

func errRateLimited(retry time.Duration) error {
	secs := int(retry.Seconds()) + 1
	p := problem(http.StatusTooManyRequests, CodeRateLimited, "Too many attempts, try again later.")
	p.header = http.Header{"Retry-After": {strconv.Itoa(secs)}}
	return p
}

func errInternal(err error) error {
	_ = err // details stay in the server log; the client gets a generic message
	return problem(http.StatusInternalServerError, CodeInternal, "Internal server error.")
}

// accountError maps an accounts service error to a problem.
func accountError(err error) error {
	var tf *accounts.TestFailedError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &tf):
		p := problem(http.StatusUnprocessableEntity, CodeAccountTestFailed, "The connection test failed.")
		p.Test = &tf.Result
		return p
	case errors.Is(err, accounts.ErrNotFound):
		return errNotFound()
	case errors.Is(err, accounts.ErrCustomServersDenied):
		return problem(http.StatusForbidden, CodeCustomServers, "This instance only allows its default mail servers.")
	case errors.Is(err, accounts.ErrHostNotAllowed):
		return problem(http.StatusUnprocessableEntity, CodeHostNotAllowed, "The mail server address is not allowed.")
	case errors.Is(err, accounts.ErrLimitReached):
		return problem(http.StatusConflict, CodeAccountLimit, "The account limit for this user is reached.")
	case errors.Is(err, accounts.ErrExists):
		return problem(http.StatusConflict, CodeAccountExists, "This account already exists.")
	case errors.Is(err, accounts.ErrInvalid):
		return problem(http.StatusUnprocessableEntity, CodeValidation, strings.TrimPrefix(err.Error(), "accounts: invalid parameters: "))
	}
	return errInternal(err)
}
