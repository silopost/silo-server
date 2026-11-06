package drafts

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/ops"
	"github.com/silopost/silo-server/internal/worker"
)

// Keywords set on the original of a sent reply or forward (plan 3.9 step 5).
const (
	flagAnswered  = `\Answered`
	flagForwarded = "$Forwarded"
)

// errNoStartTLS is the error of smtp.NewClientStartTLS when the server does
// not offer STARTTLS.
const errNoStartTLS = "smtp: server doesn't support STARTTLS"

// sendError is the outcome of a failed delivery attempt.
type sendError struct {
	temporary bool // 4xx or a network/TLS failure: retried
	msg       string
}

func (e *sendError) Error() string { return e.msg }

func permanent(format string, args ...any) *sendError {
	return &sendError{msg: fmt.Sprintf(format, args...)}
}

// classify turns an SMTP client error into a sendError: 4xx replies and
// errors without a reply (network, TLS) are temporary, 5xx are permanent.
func classify(stage string, err error) *sendError {
	var se *smtp.SMTPError
	if errors.As(err, &se) {
		return &sendError{temporary: se.Code >= 400 && se.Code < 500, msg: fmt.Sprintf("%s: %d %s", stage, se.Code, se.Message)}
	}
	return &sendError{temporary: true, msg: stage + ": " + err.Error()}
}

// smtpAccount is what the send job needs of an account.
type smtpAccount struct {
	id, userID     int64
	email          string
	host           string
	port           int
	tls            string
	username       string
	secret         []byte
	sentAppend     bool
	password       string
	insecureTransp bool
	dialCtx        func(ctx context.Context, network, addr string) (net.Conn, error)
	// oauthToken returns the access token of an xoauth2 account (nil: password).
	oauthToken func(ctx context.Context) (string, error)
}

// delivery is a successful delivery.
type delivery struct {
	response string
	rejected []string // recipients refused at RCPT with 5xx
}

// dial opens the submission connection (plan 3.9 step 4): implicit TLS
// (465), or STARTTLS, which is required unless SILO_ALLOW_INSECURE_TRANSPORT;
// smtp_tls none only with that setting. The TLS configuration is the one of
// the account check (accounts.probeSMTP): the account's host as ServerName,
// certificates verified unless SILO_ALLOW_INSECURE_TRANSPORT.
func dial(ctx context.Context, a *smtpAccount) (*smtp.Client, error) {
	addr := net.JoinHostPort(a.host, strconv.Itoa(a.port))
	tlsCfg := &tls.Config{ServerName: a.host, InsecureSkipVerify: a.insecureTransp} //nolint:gosec // opt-in via SILO_ALLOW_INSECURE_TRANSPORT
	open := func() (net.Conn, error) {
		dialCtx := a.dialCtx
		if dialCtx == nil {
			dialCtx = (&net.Dialer{}).DialContext
		}
		conn, err := dialCtx(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		if dl, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(dl)
		}
		return conn, nil
	}
	switch a.tls {
	case "implicit":
		conn, err := open()
		if err != nil {
			return nil, classify("connect", err)
		}
		tc := tls.Client(conn, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, classify("tls", err)
		}
		return smtp.NewClient(tc), nil
	case "none":
		if !a.insecureTransp {
			return nil, permanent("unencrypted SMTP is disabled on this instance")
		}
		conn, err := open()
		if err != nil {
			return nil, classify("connect", err)
		}
		return smtp.NewClient(conn), nil
	}
	conn, err := open()
	if err != nil {
		return nil, classify("connect", err)
	}
	c, err := smtp.NewClientStartTLS(conn, tlsCfg)
	if err == nil {
		return c, nil
	}
	if err.Error() != errNoStartTLS {
		return nil, classify("starttls", err)
	}
	// The server does not offer STARTTLS.
	if !a.insecureTransp {
		return nil, permanent("starttls: the server does not offer STARTTLS")
	}
	if conn, err = open(); err != nil {
		return nil, classify("connect", err)
	}
	return smtp.NewClient(conn), nil
}

// deliver runs one SMTP transaction: AUTH (PLAIN preferred, LOGIN), MAIL
// FROM the account, RCPT TO every recipient (5xx on some of them: the others
// still get the message), DATA. The message must fit the SIZE the server
// announces.
func deliver(ctx context.Context, a *smtpAccount, rcpts []string, raw []byte) (*delivery, error) {
	c, err := dial(ctx, a)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		c.CommandTimeout, c.SubmissionTimeout = time.Until(dl), time.Until(dl)
	}
	if limit, ok := c.MaxMessageSize(); ok && limit > 0 && len(raw) > limit {
		return nil, permanent("message_too_large: %d bytes, the server accepts %d", len(raw), limit)
	}
	if ok, _ := c.Extension("AUTH"); ok {
		var auth sasl.Client
		switch {
		case a.oauthToken != nil:
			var serr *sendError
			if auth, serr = xoauth2Auth(ctx, c, a); serr != nil {
				return nil, serr
			}
		case c.SupportsAuth(sasl.Plain):
			auth = sasl.NewPlainClient("", a.username, a.password)
		case c.SupportsAuth(sasl.Login):
			auth = sasl.NewLoginClient(a.username, a.password)
		default:
			_, mechs := c.Extension("AUTH")
			return nil, permanent("auth: no supported AUTH mechanism (server offers %s)", mechs)
		}
		if err := c.Auth(auth); err != nil {
			return nil, classifyAuth(err)
		}
	} else if a.username != "" {
		return nil, permanent("auth: the server does not offer AUTH")
	}
	if err := c.Mail(a.email, &smtp.MailOptions{Size: int64(len(raw))}); err != nil {
		return nil, classify("mail from", err)
	}
	out := &delivery{}
	var firstReject string
	for _, r := range rcpts {
		err := c.Rcpt(r, nil)
		if err == nil {
			continue
		}
		e := classify("rcpt to <"+r+">", err)
		if e.temporary {
			return nil, e
		}
		out.rejected = append(out.rejected, r)
		if firstReject == "" {
			firstReject = e.msg
		}
	}
	if len(out.rejected) == len(rcpts) {
		return nil, permanent("%s", firstReject)
	}
	w, err := c.Data()
	if err != nil {
		return nil, classify("data", err)
	}
	if _, err := w.Write(raw); err != nil {
		return nil, classify("data", err)
	}
	resp, err := w.CloseWithResponse()
	if err != nil {
		return nil, classify("data", err)
	}
	out.response = "250 " + resp.StatusText
	_ = c.Quit()
	return out, nil
}

// runSend is the handler of the send job.
func (s *Service) runSend(ctx context.Context, job worker.Job) error {
	var p sendPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil || p.OutboundID == 0 {
		return worker.Permanent(fmt.Errorf("drafts: bad send payload: %s", job.Payload))
	}
	return s.SendOutbound(ctx, p.OutboundID)
}

// SendOutbound delivers a queued outbound message (the send job; plan 3.9
// steps 4-6). A send cancelled meanwhile is skipped. Success: sent (see
// markSent). A temporary failure (4xx, network) puts it back in the queue
// and returns a worker.Defer with the next delay of SendRetry; after the
// last one, and at once for a permanent failure (5xx for the whole message,
// all recipients refused, SIZE exceeded, no STARTTLS), the send fails and
// the message is a draft again. It returns nil once the outcome is recorded.
func (s *Service) SendOutbound(ctx context.Context, outboundID int64) error {
	var o struct {
		accountID, userID, msgID int64
		attempts                 int
		status                   string
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT account_id, user_id, message_id, status, attempts FROM outbound WHERE id = $1 FOR UPDATE`, outboundID).
			Scan(&o.accountID, &o.userID, &o.msgID, &o.status, &o.attempts)
		if err != nil || o.status != mail.OutboundQueued {
			return err
		}
		o.attempts++
		_, err = tx.Exec(ctx, `UPDATE outbound SET status = 'sending', attempts = $2, updated_at = now() WHERE id = $1`, outboundID, o.attempts)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || err == nil && o.status != mail.OutboundQueued {
		return nil // cancelled, failed or sent meanwhile
	}
	if err != nil {
		return err
	}

	acc, m, raw, err := s.loadSend(ctx, o.accountID, o.msgID)
	if err != nil {
		// Not an SMTP outcome: give the send back to the queue and retry.
		if e := s.requeue(ctx, outboundID, err.Error()); e != nil {
			return e
		}
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, s.o.SMTPTimeout)
	d, err := deliver(sctx, acc, m.Recipients(), raw)
	cancel()
	if err == nil {
		return s.markSent(ctx, outboundID, acc, m, d)
	}
	if ctx.Err() != nil {
		// Shutdown: the job is given back; the send is queued again.
		return errors.Join(s.requeue(context.WithoutCancel(ctx), outboundID, "interrupted"), ctx.Err())
	}
	se, ok := err.(*sendError)
	if !ok {
		se = classify("smtp", err)
	}
	if se.temporary && o.attempts <= len(s.o.SendRetry) {
		delay := s.o.SendRetry[o.attempts-1]
		s.log.Warn("send failed, will retry", "outbound", outboundID, "attempt", o.attempts, "in", delay, "error", se.msg)
		if err := s.requeue(ctx, outboundID, se.msg); err != nil {
			return err
		}
		return worker.Defer(delay, se.msg)
	}
	s.log.Warn("send failed", "outbound", outboundID, "attempt", o.attempts, "error", se.msg)
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := mail.LockAccount(ctx, tx, o.accountID); err != nil {
			return err
		}
		return s.backToDraft(ctx, tx, o.accountID, o.userID, o.msgID, outboundID, mail.OutboundFailed, se.msg, "")
	})
}

// requeue puts a send that will be retried back to queued (cancel works
// again while it waits).
func (s *Service) requeue(ctx context.Context, outboundID int64, errText string) error {
	_, err := s.o.Pool.Exec(ctx, `UPDATE outbound SET status = 'queued', last_error = $2, updated_at = now() WHERE id = $1 AND status = 'sending'`, outboundID, errText)
	return err
}

// loadSend reads the account (password decrypted), the envelope (the
// draft's recipients) and the stored message of a send.
func (s *Service) loadSend(ctx context.Context, accountID, msgID int64) (*smtpAccount, *mail.DraftMIME, []byte, error) {
	a := &smtpAccount{id: accountID, insecureTransp: s.o.InsecureTransport, dialCtx: s.o.DialContext}
	var authType string
	err := s.o.Pool.QueryRow(ctx, `
SELECT user_id, email::text, smtp_host, smtp_port, smtp_tls, username, secret_enc, sent_append, auth_type FROM accounts WHERE id = $1`, accountID).
		Scan(&a.userID, &a.email, &a.host, &a.port, &a.tls, &a.username, &a.secret, &a.sentAppend, &authType)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("drafts: load account %d: %w", accountID, err)
	}
	if authType == "xoauth2" {
		if s.o.OAuth == nil {
			return nil, nil, nil, errors.New("drafts: no OAuth provider to authenticate an xoauth2 account")
		}
		a.oauthToken = func(ctx context.Context) (string, error) { return s.o.OAuth.Token(ctx, accountID) }
	} else if s.o.Keys == nil {
		return nil, nil, nil, errors.New("drafts: no keyring to decrypt the SMTP password")
	}
	if a.oauthToken == nil {
		pass, err := s.o.Keys.Decrypt(a.secret, []byte(strconv.FormatInt(accountID, 10)))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("drafts: decrypt account secret: %w", err)
		}
		a.password = string(pass)
	}
	m := &mail.DraftMIME{}
	var to, cc, bcc []byte
	var sha []byte
	err = s.o.Pool.QueryRow(ctx, `
SELECT d.to_addrs, d.cc_addrs, d.bcc_addrs, m.message_id_hdr, m.raw_blob_sha256
FROM drafts d JOIN messages m ON m.id = d.message_id WHERE d.message_id = $1`, msgID).Scan(&to, &cc, &bcc, &m.MessageID, &sha)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("drafts: load sent draft %d: %w", msgID, err)
	}
	_ = json.Unmarshal(to, &m.To)
	_ = json.Unmarshal(cc, &m.Cc)
	_ = json.Unmarshal(bcc, &m.Bcc)
	if sha == nil {
		return nil, nil, nil, fmt.Errorf("drafts: message %d has no stored body", msgID)
	}
	rc, err := s.o.Blobs.Open(ctx, hex.EncodeToString(sha))
	if err != nil {
		return nil, nil, nil, err
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	return a, m, raw, err
}

// markSent records a delivered message (plan 3.9 step 5), in one
// transaction: outbound sent (smtp_response, failed_recipients); the message
// dated now and SENT instead of SCHEDULED, its drafts row gone and its copy
// in the Drafts folder removed (delete_draft); an append job to the Sent
// folder when accounts.sent_append; \Answered (reply) or $Forwarded
// (forward) on the original; contacts.sent_count of the recipients; events.
func (s *Service) markSent(ctx context.Context, outboundID int64, a *smtpAccount, m *mail.DraftMIME, d *delivery) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := mail.LockAccount(ctx, tx, a.id); err != nil {
			return err
		}
		var msgID int64
		var lastErr *string
		if len(d.rejected) > 0 {
			e := "refused recipients: " + strings.Join(d.rejected, ", ")
			lastErr = &e
		}
		failed := d.rejected
		if failed == nil {
			failed = []string{}
		}
		if err := tx.QueryRow(ctx, `
UPDATE outbound SET status = 'sent', smtp_response = $2, failed_recipients = $3, last_error = $4, updated_at = now()
WHERE id = $1 RETURNING message_id`, outboundID, d.response, failed, lastErr).Scan(&msgID); err != nil {
			return err
		}
		var orig *int64
		var isReply bool
		if err := tx.QueryRow(ctx, `
SELECT d.in_reply_to_message_id, m.in_reply_to_hdr IS NOT NULL
FROM drafts d JOIN messages m ON m.id = d.message_id WHERE d.message_id = $1`, msgID).Scan(&orig, &isReply); err != nil {
			return err
		}
		folderID, err := removeServerCopy(ctx, tx, a.id, msgID)
		if err != nil {
			return err
		}
		if folderID != 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM message_locations WHERE folder_id = $1 AND message_id = $2`, folderID, msgID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM drafts WHERE message_id = $1`, msgID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE messages m SET internal_date = now(), header_date = now(), is_unread = false,
  dedupe_key = CASE WHEN EXISTS (SELECT 1 FROM messages x WHERE x.account_id = m.account_id AND x.dedupe_key = m.message_id_hdr || ':' || m.size_bytes)
                    THEN m.dedupe_key ELSE m.message_id_hdr || ':' || m.size_bytes END
WHERE m.id = $1`, msgID); err != nil {
			return err
		}
		if a.sentAppend {
			payload, _ := json.Marshal(mail.OpPayload{Op: mail.OpAppend, ToRole: "sent"})
			var jobID int64
			if err := tx.QueryRow(ctx, `INSERT INTO jobs (kind, account_id, payload, max_attempts) VALUES ($1, $2, $3, $4) RETURNING id`,
				ops.JobKind, a.id, payload, ops.MaxAttempts).Scan(&jobID); err != nil {
				return fmt.Errorf("drafts: enqueue append to Sent: %w", err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO op_messages (job_id, message_id, account_id, user_id) VALUES ($1, $2, $3, $4)`,
				jobID, msgID, a.id, a.userID); err != nil {
				return err
			}
		}
		refresh := []int64{msgID}
		if orig != nil {
			var located bool
			if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM message_locations l JOIN messages m ON m.id = l.message_id
               WHERE l.message_id = $1 AND m.account_id = $2)`, *orig, a.id).Scan(&located); err != nil {
				return err
			}
			if located {
				flag := flagForwarded
				if isReply {
					flag = flagAnswered
				}
				if err := ops.EnqueueFlags(ctx, tx, a.id, a.userID, []string{flag}, []int64{*orig}); err != nil {
					return err
				}
				refresh = append(refresh, *orig)
			}
		}
		if err := upsertSentContacts(ctx, tx, a, m, d.rejected); err != nil {
			return err
		}
		if _, err := mail.RefreshMessages(ctx, tx, a.id, refresh, ""); err != nil {
			return err
		}
		return writeOutboundEvent(ctx, tx, a.userID, a.id, msgID, outboundID, mail.OutboundSent, "", d.rejected, "")
	})
}

// upsertSentContacts counts a send in contacts.sent_count of the recipients
// that accepted it (plan 3.9 step 5; autocomplete ranking).
func upsertSentContacts(ctx context.Context, tx pgx.Tx, a *smtpAccount, m *mail.DraftMIME, rejected []string) error {
	seen := map[string]bool{strings.ToLower(a.email): true}
	for _, r := range rejected {
		seen[strings.ToLower(r)] = true
	}
	var emails, names []string
	for _, l := range [][]mail.Address{m.To, m.Cc, m.Bcc} {
		for _, x := range l {
			e := strings.ToLower(x.Email)
			if e == "" || seen[e] {
				continue
			}
			seen[e] = true
			emails, names = append(emails, e), append(names, x.Name)
		}
	}
	if len(emails) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
INSERT INTO contacts (user_id, email, name, sent_count, last_interaction_at)
SELECT $1, c.email, c.name, 1, now() FROM unnest($2::text[], $3::text[]) AS c(email, name)
ON CONFLICT (user_id, email) DO UPDATE SET
  sent_count = contacts.sent_count + 1,
  name = CASE WHEN contacts.name = '' THEN EXCLUDED.name ELSE contacts.name END,
  last_interaction_at = now()`, a.userID, emails, names)
	if err != nil {
		return fmt.Errorf("drafts: upsert contacts: %w", err)
	}
	return nil
}
