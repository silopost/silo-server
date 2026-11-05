package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/silopost/silo-server/internal/accounts"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
)

// Account is a mail account with its sync state. Secrets are never returned.
type Account struct {
	ID          string     `json:"id" example:"acc_1"`
	Email       string     `json:"email"`
	DisplayName string     `json:"displayName"`
	Position    int        `json:"position" doc:"Order of the account among the user's accounts (0 first); account lists are sorted by it. silo-web maps /u/<n> to the n-th account."`
	Provider    string     `json:"provider" enum:"generic,gmail,microsoft"`
	Username    string     `json:"username"`
	IMAPHost    string     `json:"imapHost"`
	IMAPPort    int        `json:"imapPort"`
	IMAPTLS     string     `json:"imapTls" enum:"implicit,starttls,none"`
	SMTPHost    string     `json:"smtpHost"`
	SMTPPort    int        `json:"smtpPort"`
	SMTPTLS     string     `json:"smtpTls" enum:"implicit,starttls,none"`
	SentAppend  bool       `json:"sentAppend" doc:"Whether Silo copies sent messages to the Sent folder itself (servers that do not)."`
	State       string     `json:"state" enum:"connecting,discovering,initial_sync,live,backoff,auth_failed,disabled" doc:"Sync state; backoff means the mail server is unreachable."`
	Error       *string    `json:"error"`
	LastSyncAt  *time.Time `json:"lastSyncAt"`
}

func accountOf(a sqlcgen.Account) Account {
	out := Account{
		ID: formatID("acc_", a.ID), Email: a.Email, DisplayName: a.DisplayName, Position: int(a.Position), Provider: a.Provider, Username: a.Username,
		IMAPHost: a.ImapHost, IMAPPort: int(a.ImapPort), IMAPTLS: a.ImapTls,
		SMTPHost: a.SmtpHost, SMTPPort: int(a.SmtpPort), SMTPTLS: a.SmtpTls,
		SentAppend: a.SentAppend, State: a.SyncState,
	}
	if a.SyncError.Valid {
		out.Error = &a.SyncError.String
	}
	if a.LastSyncAt.Valid {
		out.LastSyncAt = &a.LastSyncAt.Time
	}
	return out
}

func accountList(accs []sqlcgen.Account) []Account {
	out := make([]Account, 0, len(accs))
	for _, a := range accs {
		out = append(out, accountOf(a))
	}
	return out
}

// ConnectionParams are the server settings and credentials of an account.
type ConnectionParams struct {
	Email    string `json:"email,omitempty" maxLength:"254" doc:"Sender address."`
	Username string `json:"username,omitempty" doc:"Login name; defaults to email."`
	Password string `json:"password" minLength:"1" maxLength:"1024"`
	IMAPHost string `json:"imapHost,omitempty" doc:"Defaults to the default server of the instance."`
	IMAPPort int    `json:"imapPort,omitempty" minimum:"1" maximum:"65535"`
	IMAPTLS  string `json:"imapTls,omitempty" enum:"implicit,starttls,none"`
	SMTPHost string `json:"smtpHost,omitempty"`
	SMTPPort int    `json:"smtpPort,omitempty" minimum:"1" maximum:"65535"`
	SMTPTLS  string `json:"smtpTls,omitempty" enum:"implicit,starttls,none"`
}

func (p ConnectionParams) connection() accounts.Connection {
	return accounts.Connection{
		Email: p.Email, Username: p.Username, Password: p.Password,
		IMAPHost: p.IMAPHost, IMAPPort: p.IMAPPort, IMAPTLS: p.IMAPTLS,
		SMTPHost: p.SMTPHost, SMTPPort: p.SMTPPort, SMTPTLS: p.SMTPTLS,
	}
}

// CreateAccount is the body of POST /accounts.
type CreateAccount struct {
	ConnectionParams
	DisplayName string `json:"displayName,omitempty" maxLength:"200"`
}

// PatchAccount is the body of PATCH /accounts/{id}; omitted fields stay as they are.
type PatchAccount struct {
	DisplayName *string `json:"displayName,omitempty" maxLength:"200"`
	Password    *string `json:"password,omitempty" minLength:"1" maxLength:"1024"`
	IMAPPort    *int    `json:"imapPort,omitempty" minimum:"1" maximum:"65535"`
	IMAPTLS     *string `json:"imapTls,omitempty" enum:"implicit,starttls,none"`
	SMTPPort    *int    `json:"smtpPort,omitempty" minimum:"1" maximum:"65535"`
	SMTPTLS     *string `json:"smtpTls,omitempty" enum:"implicit,starttls,none"`
	SentAppend  *bool   `json:"sentAppend,omitempty"`
}

type accountIDInput struct {
	ID string `path:"id" example:"acc_1"`
}

type patchAccountInput struct {
	ID   string `path:"id" example:"acc_1"`
	Body PatchAccount
}

type accountsOutput struct {
	Body struct {
		Accounts []Account `json:"accounts"`
	}
}

type accountOutput struct{ Body Account }

type testOutput struct{ Body accounts.TestResult }

func (d *deps) registerAccounts(a huma.API) {
	errs := func(more ...int) []int { return append([]int{http.StatusUnauthorized, http.StatusForbidden}, more...) }

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "listAccounts", Method: http.MethodGet, Path: "/accounts", Tags: []string{"accounts"},
		Summary: "List the accounts of the user with their sync state", Errors: []int{http.StatusUnauthorized},
	}), func(ctx context.Context, _ *struct{}) (*accountsOutput, error) {
		accs, err := d.accts.List(ctx, sessionFrom(ctx).UserID)
		if err != nil {
			return nil, errInternal(err)
		}
		out := &accountsOutput{}
		out.Body.Accounts = accountList(accs)
		return out, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "testAccount", Method: http.MethodPost, Path: "/accounts/test", Tags: []string{"accounts"},
		Summary: "Test a connection without saving",
		Description: "Runs IMAP (connect, tls, login, capability, list) and SMTP (connect, ehlo, starttls, auth) checks " +
			"and reports each step; nothing is sent or stored. The response is 200 even when a step fails: " +
			"inspect `ok` per protocol.",
		Errors: errs(http.StatusUnprocessableEntity),
	}), func(ctx context.Context, in *struct{ Body ConnectionParams }) (*testOutput, error) {
		res, err := d.accts.Test(ctx, in.Body.connection())
		if err != nil {
			return nil, accountError(err)
		}
		return &testOutput{Body: res}, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "createAccount", Method: http.MethodPost, Path: "/accounts", Tags: []string{"accounts"},
		Summary:       "Add an account",
		Description:   "Runs the connection test first; a failure answers 422 account_test_failed with the steps in `test`.",
		DefaultStatus: http.StatusCreated,
		Errors:        errs(http.StatusUnprocessableEntity, http.StatusConflict),
	}), func(ctx context.Context, in *struct{ Body CreateAccount }) (*accountOutput, error) {
		acc, err := d.accts.Create(ctx, sessionFrom(ctx).UserID, accounts.Create{
			Connection: in.Body.connection(), DisplayName: in.Body.DisplayName,
		})
		if err != nil {
			return nil, accountError(err)
		}
		return &accountOutput{Body: accountOf(acc)}, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "updateAccount", Method: http.MethodPatch, Path: "/accounts/{id}", Tags: []string{"accounts"},
		Summary: "Change account settings", Errors: errs(http.StatusNotFound, http.StatusUnprocessableEntity),
	}), func(ctx context.Context, in *patchAccountInput) (*accountOutput, error) {
		id, ok := parseID("acc_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		b := in.Body
		acc, err := d.accts.Update(ctx, sessionFrom(ctx).UserID, id, accounts.Update{
			DisplayName: b.DisplayName, Password: b.Password, IMAPPort: b.IMAPPort, IMAPTLS: b.IMAPTLS,
			SMTPPort: b.SMTPPort, SMTPTLS: b.SMTPTLS, SentAppend: b.SentAppend,
		})
		if err != nil {
			return nil, accountError(err)
		}
		return &accountOutput{Body: accountOf(acc)}, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "deleteAccount", Method: http.MethodDelete, Path: "/accounts/{id}", Tags: []string{"accounts"},
		Summary:       "Remove an account from Silo",
		Description:   "Deletes the account and everything cached for it. The mail server is not touched.",
		DefaultStatus: http.StatusNoContent,
		Errors:        errs(http.StatusNotFound),
	}), func(ctx context.Context, in *accountIDInput) (*struct{}, error) {
		id, ok := parseID("acc_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		if err := d.accts.Delete(ctx, sessionFrom(ctx).UserID, id); err != nil {
			return nil, accountError(err)
		}
		return nil, nil
	})
}
