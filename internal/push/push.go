// Package push sends push notifications of new mail to native clients
// (phase 6) through the Expo Push Service, and keeps the devices that
// receive them (push_devices).
//
// A device belongs to one bearer session (ADR 0007): signing out ends its
// push. New INBOX messages queue push.notify (mail.EnqueuePushNotify, from
// the incremental sync and from filter_eval); the job checks again what is
// still worth a notification and sends one message per device and account,
// without content: a generic, localized title and the ids the app needs to
// open the mail. push.receipts reads the delivery receipts 15 minutes later.
package push

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/auth"
	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
)

// Errors of the device operations.
var (
	// ErrDisabled: push is switched off on the instance (SILO_PUSH_ENABLED).
	ErrDisabled = errors.New("push: disabled on this instance")
	// ErrNotBearer: only a bearer session (a native client) has a device.
	ErrNotBearer = errors.New("push: a bearer session is required")
	// ErrInvalidAccount: accountIds names an account the user does not have.
	ErrInvalidAccount = errors.New("push: unknown account")
	// ErrInvalid: provider or token not accepted.
	ErrInvalid = errors.New("push: invalid device")
	// ErrNotFound: no such device of the user (or of the session).
	ErrNotFound = errors.New("push: device not found")
)

// MaxTokenLength bounds a push token; Expo tokens are far shorter.
const MaxTokenLength = 255

// expoToken is the shape of an Expo push token.
var expoToken = regexp.MustCompile(`^Expo(nent)?PushToken\[[^\[\]\s]{1,200}\]$`)

// ValidToken reports whether token is an Expo push token.
func ValidToken(token string) bool {
	return len(token) <= MaxTokenLength && expoToken.MatchString(token)
}

// Options configure a Service.
type Options struct {
	Pool   *pgxpool.Pool
	Config config.PushConfig
	// HTTPClient talks to the Expo Push Service (a client with a 15 s timeout when nil).
	HTTPClient *http.Client
	// ReceiptDelay is when push.receipts runs after a send [15m].
	ReceiptDelay time.Duration
	Logger       *slog.Logger
}

// Service registers devices and runs the push jobs.
type Service struct {
	o    Options
	expo *expoClient
	log  *slog.Logger
}

// New returns a Service.
func New(o Options) *Service {
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if o.ReceiptDelay <= 0 {
		o.ReceiptDelay = 15 * time.Minute
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		o:    o,
		expo: &expoClient{http: o.HTTPClient, sendURL: o.Config.SendURL(), receiptsURL: o.Config.ReceiptsURL()},
		log:  log.With("component", "push"),
	}
}

// Enabled reports whether push is switched on.
func (s *Service) Enabled() bool { return s != nil && s.o.Config.Enabled }

// Providers lists the push providers clients may register with; never nil.
func (s *Service) Providers() []string {
	if s == nil {
		return []string{}
	}
	return s.o.Config.Providers()
}

// Registration is the device a session registers.
type Registration struct {
	Provider string
	Token    string
	// AccountIDs limits push to these accounts of the user; nil: all.
	AccountIDs []int64
}

// Register registers (or updates) the device of a bearer session. The same
// token registered before, by any session, moves to this one; a session
// that registers another token replaces its device.
func (s *Service) Register(ctx context.Context, sess sqlcgen.Session, r Registration) (sqlcgen.PushDevice, error) {
	if !s.Enabled() {
		return sqlcgen.PushDevice{}, ErrDisabled
	}
	if sess.Kind != auth.KindBearer {
		return sqlcgen.PushDevice{}, ErrNotBearer
	}
	if r.Provider != config.PushExpo || !ValidToken(r.Token) {
		return sqlcgen.PushDevice{}, ErrInvalid
	}
	var ids []int64
	if r.AccountIDs != nil {
		ids = slices.Clone(r.AccountIDs)
		slices.Sort(ids)
		ids = slices.Compact(ids)
	}
	var dev sqlcgen.PushDevice
	err := pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if len(ids) > 0 {
			n, err := q.CountUserAccountsIn(ctx, sqlcgen.CountUserAccountsInParams{UserID: sess.UserID, Ids: ids})
			if err != nil {
				return err
			}
			if int(n) != len(ids) {
				return ErrInvalidAccount
			}
		}
		if err := q.DeleteSessionPushDeviceExcept(ctx, sqlcgen.DeleteSessionPushDeviceExceptParams{
			SessionID: sess.ID, Provider: r.Provider, PushToken: r.Token,
		}); err != nil {
			return err
		}
		var err error
		dev, err = q.UpsertPushDevice(ctx, sqlcgen.UpsertPushDeviceParams{
			UserID: sess.UserID, SessionID: sess.ID, Provider: r.Provider, PushToken: r.Token, AccountIds: ids,
		})
		return err
	})
	if err != nil {
		if errors.Is(err, ErrInvalidAccount) {
			return sqlcgen.PushDevice{}, err
		}
		return sqlcgen.PushDevice{}, fmt.Errorf("push: register device: %w", err)
	}
	s.log.Info("push device registered", "device", dev.ID, "user", sess.UserID, "token", RedactToken(r.Token))
	return dev, nil
}

// Current returns the device of a bearer session.
func (s *Service) Current(ctx context.Context, sess sqlcgen.Session) (sqlcgen.PushDevice, error) {
	if s == nil {
		return sqlcgen.PushDevice{}, ErrDisabled
	}
	if sess.Kind != auth.KindBearer {
		return sqlcgen.PushDevice{}, ErrNotBearer
	}
	dev, err := sqlcgen.New(s.o.Pool).GetSessionPushDevice(ctx, sess.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.PushDevice{}, ErrNotFound
	}
	if err != nil {
		return sqlcgen.PushDevice{}, fmt.Errorf("push: load device: %w", err)
	}
	return dev, nil
}

// Delete removes a device of the user.
func (s *Service) Delete(ctx context.Context, userID, id int64) error {
	if s == nil {
		return ErrDisabled
	}
	n, err := sqlcgen.New(s.o.Pool).DeletePushDevice(ctx, sqlcgen.DeletePushDeviceParams{ID: id, UserID: userID})
	if err != nil {
		return fmt.Errorf("push: delete device: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RedactToken shortens a push token for logs: the token is a credential for
// sending to the device.
func RedactToken(token string) string {
	const keep = 6
	if len(token) <= 2*keep {
		return "…"
	}
	return token[:keep] + "…" + token[len(token)-2:]
}
