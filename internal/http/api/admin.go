package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/silopost/silo-server/internal/admin"
	"github.com/silopost/silo-server/internal/auth"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
)

// AdminUserAccount is a mail account as listed with its user.
type AdminUserAccount struct {
	ID         string     `json:"id" example:"acc_1"`
	Email      string     `json:"email"`
	State      string     `json:"state" enum:"connecting,discovering,initial_sync,live,backoff,auth_failed,disabled"`
	LastSyncAt *time.Time `json:"lastSyncAt"`
	Error      *string    `json:"error"`
}

// AdminUser is a user of the instance as the admin panel shows it.
type AdminUser struct {
	ID          string             `json:"id" example:"u_1"`
	Email       string             `json:"email"`
	DisplayName string             `json:"displayName"`
	IsAdmin     bool               `json:"isAdmin"`
	Disabled    bool               `json:"disabled" doc:"A disabled user cannot sign in; their sessions and account syncs are stopped."`
	TOTPEnabled bool               `json:"totpEnabled"`
	CreatedAt   time.Time          `json:"createdAt"`
	Accounts    []AdminUserAccount `json:"accounts"`
}

// AdminCreatedUser is the response of POST /admin/users.
type AdminCreatedUser struct {
	AdminUser
	TemporaryPassword string `json:"temporaryPassword,omitempty" doc:"Generated password when none was given; shown only this once."`
}

// AdminAccount is a mail account of any user with its sync and queue state.
type AdminAccount struct {
	ID          string     `json:"id" example:"acc_1"`
	UserID      string     `json:"userId" example:"u_1"`
	UserEmail   string     `json:"userEmail"`
	Email       string     `json:"email"`
	DisplayName string     `json:"displayName"`
	IMAPHost    string     `json:"imapHost"`
	AuthType    string     `json:"authType" enum:"password,xoauth2" doc:"How Silo signs in to the servers (see Account.authType)."`
	State       string     `json:"state" enum:"connecting,discovering,initial_sync,live,backoff,auth_failed,disabled"`
	Error       *string    `json:"error"`
	LastSyncAt  *time.Time `json:"lastSyncAt"`
	PendingOps  int        `json:"pendingOps" doc:"Operations waiting for write-back to the mail server."`
	DeadOps     int        `json:"deadOps" doc:"Jobs of the account that failed for good (see GET /admin/jobs?status=dead)."`
	CreatedAt   time.Time  `json:"createdAt"`
}

// AdminJob is a queued job (its payload is not exposed).
type AdminJob struct {
	ID          string    `json:"id" example:"job_1"`
	Kind        string    `json:"kind" example:"imap_op"`
	AccountID   *string   `json:"accountId" doc:"Account the job belongs to; null for instance-wide jobs (sending, cleanup)."`
	UserID      *string   `json:"userId" doc:"Owner of the account; null for instance-wide jobs."`
	Status      string    `json:"status" enum:"pending,running,done,dead" doc:"pending also covers a job being run (claimed by a worker)."`
	Attempts    int       `json:"attempts"`
	MaxAttempts int       `json:"maxAttempts"`
	LastError   *string   `json:"lastError"`
	RunAt       time.Time `json:"runAt"`
	CreatedAt   time.Time `json:"createdAt"`
}

func adminUserOf(v admin.UserView) AdminUser {
	u := v.User
	out := AdminUser{
		ID: formatID("u_", u.ID), Email: u.Email, DisplayName: u.DisplayName, IsAdmin: u.IsAdmin,
		Disabled: u.DisabledAt.Valid, TOTPEnabled: u.TotpEnabledAt.Valid, CreatedAt: u.CreatedAt.Time,
		Accounts: make([]AdminUserAccount, 0, len(v.Accounts)),
	}
	for _, a := range v.Accounts {
		acc := AdminUserAccount{ID: formatID("acc_", a.ID), Email: a.Email, State: a.SyncState}
		if a.LastSyncAt.Valid {
			acc.LastSyncAt = &a.LastSyncAt.Time
		}
		if a.SyncError.Valid {
			acc.Error = &a.SyncError.String
		}
		out.Accounts = append(out.Accounts, acc)
	}
	return out
}

func adminJobOf(j sqlcgen.AdminListJobsRow) AdminJob {
	out := AdminJob{
		ID: formatID("job_", j.ID), Kind: j.Kind, Status: j.Status, Attempts: int(j.Attempts),
		MaxAttempts: int(j.MaxAttempts), RunAt: j.RunAt.Time, CreatedAt: j.CreatedAt.Time,
	}
	if j.AccountID.Valid {
		id := formatID("acc_", j.AccountID.Int64)
		out.AccountID = &id
	}
	if j.AccountUserID.Valid {
		id := formatID("u_", j.AccountUserID.Int64)
		out.UserID = &id
	}
	if j.LastError.Valid {
		out.LastError = &j.LastError.String
	}
	return out
}

// adminError maps admin and auth errors to problems.
func adminError(err error) error {
	switch {
	case errors.Is(err, admin.ErrNotFound):
		return errNotFound()
	case errors.Is(err, admin.ErrSelf):
		return problem(http.StatusConflict, CodeSelfChange, "Admins cannot disable, delete or demote themselves.")
	}
	return authError(err)
}

// requireAdmin lets only users with users.is_admin through (403 forbidden,
// not 404: the endpoints are documented). It runs after requireSession.
func (d *deps) requireAdmin(ctx huma.Context, next func(huma.Context)) {
	ok, err := d.admin.IsAdmin(ctx.Context(), sessionFrom(ctx.Context()).UserID)
	if err != nil {
		writeProblem(ctx, errInternal(err).(*Problem))
		return
	}
	if !ok {
		writeProblem(ctx, problem(http.StatusForbidden, CodeForbidden, "Admin rights are required."))
		return
	}
	next(ctx)
}

// adminOp is a secured operation for admins only.
func (d *deps) adminOp(op huma.Operation) huma.Operation {
	op = d.secured(op)
	op.Tags = []string{"admin"}
	op.Middlewares = append(op.Middlewares, d.requireAdmin)
	op.Errors = append(op.Errors, http.StatusUnauthorized, http.StatusForbidden)
	return op
}

type adminUsersOutput struct {
	Body struct {
		Users []AdminUser `json:"users"`
	}
}

type adminUserOutput struct{ Body AdminUser }

type adminCreateUserInput struct {
	Body struct {
		Email       string  `json:"email" minLength:"3" maxLength:"254" format:"email"`
		DisplayName string  `json:"displayName,omitempty" maxLength:"200"`
		IsAdmin     bool    `json:"isAdmin,omitempty"`
		Password    *string `json:"password,omitempty" minLength:"10" maxLength:"1024" doc:"Silo password; generated and returned once as temporaryPassword when omitted."`
	}
}

type adminCreateUserOutput struct{ Body AdminCreatedUser }

type adminUserIDInput struct {
	ID string `path:"id" example:"u_1"`
}

type adminPatchUserInput struct {
	ID   string `path:"id" example:"u_1"`
	Body struct {
		Disabled    *bool   `json:"disabled,omitempty" doc:"true signs the user out everywhere and stops their account syncs; false restarts them."`
		IsAdmin     *bool   `json:"isAdmin,omitempty"`
		DisplayName *string `json:"displayName,omitempty" maxLength:"200"`
		Password    *string `json:"password,omitempty" minLength:"10" maxLength:"1024" doc:"New Silo password (mode local); signs the user out everywhere."`
	}
}

type adminAccountsOutput struct {
	Body struct {
		Accounts []AdminAccount `json:"accounts"`
	}
}

type adminJobsInput struct {
	Status    string `query:"status" enum:"pending,dead" doc:"Only jobs in this state; all when omitted."`
	AccountID string `query:"accountId" example:"acc_1" doc:"Only jobs of this account."`
}

type adminJobsOutput struct {
	Body struct {
		Jobs []AdminJob `json:"jobs"`
	}
}

type adminJobIDInput struct {
	ID string `path:"id" example:"job_1"`
}

type adminJobOutput struct{ Body AdminJob }

// registerAdmin adds the admin panel endpoints (both auth modes; users with
// users.is_admin only).
func (d *deps) registerAdmin(a huma.API) {
	huma.Register(a, d.adminOp(huma.Operation{
		OperationID: "adminListUsers", Method: http.MethodGet, Path: "/admin/users",
		Summary: "List users with their accounts",
	}), func(ctx context.Context, _ *struct{}) (*adminUsersOutput, error) {
		list, err := d.admin.Users(ctx)
		if err != nil {
			return nil, errInternal(err)
		}
		out := &adminUsersOutput{}
		out.Body.Users = make([]AdminUser, 0, len(list))
		for _, v := range list {
			out.Body.Users = append(out.Body.Users, adminUserOf(v))
		}
		return out, nil
	})

	huma.Register(a, d.adminOp(huma.Operation{
		OperationID: "adminCreateUser", Method: http.MethodPost, Path: "/admin/users",
		Summary: "Create a user", DefaultStatus: http.StatusCreated,
		Description: "Mode local only (409 not_available in mode imap, where users appear on their first sign-in). " +
			"No mail account is created; the user adds accounts after signing in. 409 user_exists for a taken email.",
		Errors: []int{http.StatusBadRequest, http.StatusConflict},
	}), func(ctx context.Context, in *adminCreateUserInput) (*adminCreateUserOutput, error) {
		n := auth.NewUser{Email: in.Body.Email, DisplayName: in.Body.DisplayName, IsAdmin: in.Body.IsAdmin}
		if in.Body.Password != nil {
			n.Password = *in.Body.Password
		}
		v, pw, err := d.admin.CreateUser(ctx, n)
		if err != nil {
			return nil, adminError(err)
		}
		return &adminCreateUserOutput{Body: AdminCreatedUser{AdminUser: adminUserOf(v), TemporaryPassword: pw}}, nil
	})

	huma.Register(a, d.adminOp(huma.Operation{
		OperationID: "adminUpdateUser", Method: http.MethodPatch, Path: "/admin/users/{id}",
		Summary: "Change a user",
		Description: "Disable or enable, grant or revoke admin rights, rename, set a new password (mode local; " +
			"409 not_available in mode imap). Admins cannot disable or demote themselves (409 admin_self_change).",
		Errors: []int{http.StatusBadRequest, http.StatusNotFound, http.StatusConflict},
	}), func(ctx context.Context, in *adminPatchUserInput) (*adminUserOutput, error) {
		id, ok := parseID("u_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		sess := sessionFrom(ctx)
		v, err := d.admin.UpdateUser(ctx, sess.UserID, sess.ID, id, admin.UserPatch{
			Disabled: in.Body.Disabled, IsAdmin: in.Body.IsAdmin, DisplayName: in.Body.DisplayName, Password: in.Body.Password,
		})
		if err != nil {
			return nil, adminError(err)
		}
		return &adminUserOutput{Body: adminUserOf(v)}, nil
	})

	huma.Register(a, d.adminOp(huma.Operation{
		OperationID: "adminResetUserTotp", Method: http.MethodPost, Path: "/admin/users/{id}/totp/reset",
		Summary: "Turn a user's TOTP off", DefaultStatus: http.StatusNoContent,
		Description: "For a lost device: the user signs in with the password alone and may set TOTP up again.",
		Errors:      []int{http.StatusNotFound},
	}), func(ctx context.Context, in *adminUserIDInput) (*struct{}, error) {
		id, ok := parseID("u_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		return nil, adminError(d.admin.ResetTOTP(ctx, id))
	})

	huma.Register(a, d.adminOp(huma.Operation{
		OperationID: "adminDeleteUser", Method: http.MethodDelete, Path: "/admin/users/{id}",
		Summary: "Delete a user", DefaultStatus: http.StatusNoContent,
		Description: "Removes the user with their accounts and everything Silo cached for them; mail servers are not touched. " +
			"Admins cannot delete themselves (409 admin_self_change).",
		Errors: []int{http.StatusNotFound, http.StatusConflict},
	}), func(ctx context.Context, in *adminUserIDInput) (*struct{}, error) {
		id, ok := parseID("u_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		return nil, adminError(d.admin.DeleteUser(ctx, sessionFrom(ctx).UserID, id))
	})

	huma.Register(a, d.adminOp(huma.Operation{
		OperationID: "adminListAccounts", Method: http.MethodGet, Path: "/admin/accounts",
		Summary: "List every mail account with its state",
	}), func(ctx context.Context, _ *struct{}) (*adminAccountsOutput, error) {
		rows, err := d.admin.Accounts(ctx)
		if err != nil {
			return nil, errInternal(err)
		}
		out := &adminAccountsOutput{}
		out.Body.Accounts = make([]AdminAccount, 0, len(rows))
		for _, r := range rows {
			acc := AdminAccount{
				ID: formatID("acc_", r.ID), UserID: formatID("u_", r.UserID), UserEmail: r.UserEmail,
				Email: r.Email, DisplayName: r.DisplayName, IMAPHost: r.ImapHost, AuthType: r.AuthType, State: r.SyncState,
				PendingOps: int(r.PendingOps), DeadOps: int(r.DeadOps), CreatedAt: r.CreatedAt.Time,
			}
			if r.SyncError.Valid {
				acc.Error = &r.SyncError.String
			}
			if r.LastSyncAt.Valid {
				acc.LastSyncAt = &r.LastSyncAt.Time
			}
			out.Body.Accounts = append(out.Body.Accounts, acc)
		}
		return out, nil
	})

	huma.Register(a, d.adminOp(huma.Operation{
		OperationID: "adminListJobs", Method: http.MethodGet, Path: "/admin/jobs",
		Summary:     "List queued jobs",
		Description: "The newest 200 jobs matching the filters, without their payloads.",
		Errors:      []int{http.StatusBadRequest},
	}), func(ctx context.Context, in *adminJobsInput) (*adminJobsOutput, error) {
		f := admin.JobFilter{Status: in.Status}
		if in.AccountID != "" {
			id, ok := parseID("acc_", in.AccountID)
			if !ok {
				return nil, problem(http.StatusBadRequest, CodeValidation, "accountId must be an account id (acc_...).")
			}
			f.AccountID = id
		}
		rows, err := d.admin.Jobs(ctx, f)
		if err != nil {
			return nil, errInternal(err)
		}
		out := &adminJobsOutput{}
		out.Body.Jobs = make([]AdminJob, 0, len(rows))
		for _, r := range rows {
			out.Body.Jobs = append(out.Body.Jobs, adminJobOf(r))
		}
		return out, nil
	})

	huma.Register(a, d.adminOp(huma.Operation{
		OperationID: "adminRetryJob", Method: http.MethodPost, Path: "/admin/jobs/{id}/retry",
		Summary:     "Run a job again",
		Description: "A dead (or waiting) job becomes pending, due now, with its attempt count reset. 404 for running or unknown jobs.",
		Errors:      []int{http.StatusNotFound},
	}), func(ctx context.Context, in *adminJobIDInput) (*adminJobOutput, error) {
		id, ok := parseID("job_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		j, err := d.admin.RetryJob(ctx, id)
		if err != nil {
			return nil, adminError(err)
		}
		return &adminJobOutput{Body: adminJobOf(j)}, nil
	})

	huma.Register(a, d.adminOp(huma.Operation{
		OperationID: "adminDeleteJob", Method: http.MethodDelete, Path: "/admin/jobs/{id}",
		Summary: "Discard a job", DefaultStatus: http.StatusNoContent,
		Description: "Drops a job that is not running, e.g. a dead write-back operation (its local change is given up). " +
			"404 for running or unknown jobs.",
		Errors: []int{http.StatusNotFound},
	}), func(ctx context.Context, in *adminJobIDInput) (*struct{}, error) {
		id, ok := parseID("job_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		return nil, adminError(d.admin.DeleteJob(ctx, id))
	})
}
