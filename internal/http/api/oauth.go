package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/silopost/silo-server/internal/accounts"
	"github.com/silopost/silo-server/internal/oauth"
)

// Error codes of the OAuth callback, sent to the web client in the redirect
// (?oauth=error&code=...) together with oauth_denied, oauth_state_invalid,
// oauth_exchange_failed and oauth_no_email of package oauth,
// account_limit_reached and account_test_failed.
const (
	CodeOAuthAccountMismatch = "oauth_account_mismatch"
)

// oauthCookie binds a started authorization to the browser that started it:
// without it, anyone could start one under their own Silo user and lure a
// victim into consenting, which would add the victim's mailbox to the
// attacker's user (OAuth login CSRF, RFC 6749 10.12).
const oauthCookie = "silo_oauth_state"

// OAuthStart is the body of POST /oauth/{provider}/start.
type OAuthStart struct {
	AccountID string `json:"accountId,omitempty" example:"acc_1" doc:"An OAuth account of the user to authorize again (after auth_failed); its address must be the one signed in with. Omitted: add an account."`
	ReturnTo  string `json:"returnTo,omitempty" maxLength:"1024" example:"/settings/accounts" doc:"Path of silo-web the browser returns to after the callback (default /); a relative path starting with a single /."`
}

type oauthStartInput struct {
	Provider string      `path:"provider" example:"google" doc:"google or microsoft (as listed by GET /meta oauthProviders)."`
	Body     *OAuthStart `required:"false"`
}

type oauthStartOutput struct {
	SetCookie string `header:"Set-Cookie" doc:"Binds the authorization to this browser (read back by the callback)."`
	Body      struct {
		URL   string `json:"url" doc:"Authorization URL of the provider; open it in the browser (top-level navigation)."`
		State string `json:"state" doc:"Opaque state of this authorization (valid 10 minutes, single use)."`
	}
}

type oauthCallbackInput struct {
	Provider         string `path:"provider"`
	Code             string `query:"code"`
	State            string `query:"state"`
	Error            string `query:"error" doc:"Set by the provider instead of code when the user declined or the request failed."`
	ErrorDescription string `query:"error_description"`
	Bound            string `cookie:"silo_oauth_state"`
}

type oauthCallbackOutput struct {
	Location     string `header:"Location"`
	SetCookie    string `header:"Set-Cookie"`
	CacheControl string `header:"Cache-Control"`
}

func (d *deps) registerOAuth(a huma.API, o Options) {
	svc := o.OAuth
	publicURL := strings.TrimRight(o.PublicURL, "/")
	stateCookie := func(value string, maxAge int) string {
		c := http.Cookie{Name: oauthCookie, Value: value, Path: Prefix + "/oauth/", MaxAge: maxAge,
			HttpOnly: true, Secure: d.cookieSecure, SameSite: http.SameSiteLaxMode}
		return c.String()
	}

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "startOAuth", Method: http.MethodPost, Path: "/oauth/{provider}/start", Tags: []string{"accounts"},
		Summary: "Start adding (or re-authorizing) an account with OAuth2",
		Description: "Returns the provider's authorization URL (authorization code flow with PKCE). The browser opens it; " +
			"the provider sends it back to GET /oauth/{provider}/callback, which adds the account and redirects to " +
			"SILO_PUBLIC_URL + returnTo. 404 when the provider is not configured on the instance, or for an accountId " +
			"that is not an OAuth account of the user with this provider.",
		Errors: []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound},
	}), func(ctx context.Context, in *oauthStartInput) (*oauthStartOutput, error) {
		p, ok := svc.Provider(in.Provider)
		if !ok {
			return nil, errNotFound()
		}
		body := OAuthStart{}
		if in.Body != nil {
			body = *in.Body
		}
		sess := sessionFrom(ctx)
		req := oauth.StartRequest{UserID: sess.UserID, ReturnTo: body.ReturnTo}
		if body.AccountID != "" {
			id, ok := parseID("acc_", body.AccountID)
			if !ok {
				return nil, errNotFound()
			}
			acc, err := d.accts.Get(ctx, sess.UserID, id)
			if err != nil {
				return nil, accountError(err)
			}
			if acc.AuthType != accounts.AuthXOAuth2 || acc.Provider != p.AccountProvider {
				return nil, errNotFound()
			}
			req.AccountID, req.LoginHint = id, acc.Username
		}
		authURL, state, err := svc.Start(ctx, in.Provider, req)
		if errors.Is(err, oauth.ErrInvalidReturnTo) {
			return nil, problem(http.StatusBadRequest, CodeValidation, "returnTo must be a path of Silo starting with a single /.")
		}
		if err != nil {
			return nil, errInternal(err)
		}
		out := &oauthStartOutput{SetCookie: stateCookie(state, int(oauth.StateTTL.Seconds()))}
		out.Body.URL, out.Body.State = authURL, state
		return out, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "oauthCallback", Method: http.MethodGet, Path: "/oauth/{provider}/callback", Tags: []string{"accounts"},
		Summary: "OAuth2 redirect URI (browser only)",
		Description: "The provider redirects the browser here; no session is needed (the state identifies the user, " +
			"the cookie set by start the browser). The code is exchanged, the account tested with XOAUTH2 and created " +
			"(or its authorization renewed), then 303 to SILO_PUBLIC_URL + returnTo with `?oauth=ok&accountId=acc_X` or " +
			"`?oauth=error&code=<code>`: oauth_denied, oauth_state_invalid, oauth_exchange_failed, oauth_no_email, " +
			"oauth_account_mismatch, account_limit_reached, account_test_failed, internal_error. " +
			"The admin registers SILO_PUBLIC_URL + /api/v1/oauth/{provider}/callback with the provider.",
		DefaultStatus: http.StatusSeeOther,
		Errors:        []int{http.StatusNotFound},
	}, func(ctx context.Context, in *oauthCallbackInput) (*oauthCallbackOutput, error) {
		if _, ok := svc.Provider(in.Provider); !ok {
			return nil, errNotFound()
		}
		out := &oauthCallbackOutput{SetCookie: stateCookie("", -1), CacheControl: "no-store"}
		redirect := func(returnTo string, q url.Values) (*oauthCallbackOutput, error) {
			u, err := url.Parse(publicURL + returnTo)
			if err != nil {
				u = &url.URL{Path: "/"}
			}
			v := u.Query()
			for k, vs := range q {
				v[k] = vs
			}
			u.RawQuery = v.Encode()
			out.Location = u.String()
			return out, nil
		}
		fail := func(returnTo, code string, err error) (*oauthCallbackOutput, error) {
			d.log.Warn("oauth callback failed", "provider", in.Provider, "code", code, "error", err)
			return redirect(returnTo, url.Values{"oauth": {"error"}, "code": {code}})
		}
		if in.State == "" || subtle.ConstantTimeCompare([]byte(in.Bound), []byte(in.State)) != 1 {
			// Not started in this browser: leave the state alone.
			return fail("/", oauth.CodeStateInvalid, errors.New("the state cookie does not match"))
		}
		g, err := svc.Complete(ctx, in.Provider, in.State, in.Code, in.Error)
		if err != nil {
			var fe *oauth.FlowError
			if errors.As(err, &fe) {
				return fail(g.ReturnTo, fe.Code, err)
			}
			return fail(g.ReturnTo, CodeInternal, err)
		}
		acc, _, err := d.accts.SaveOAuth(ctx, g.UserID, accounts.OAuthAccount{
			Provider: g.Provider.AccountProvider, Email: g.Email, DisplayName: g.Name,
			RefreshToken: g.Token.RefreshToken, AccessToken: g.Token.AccessToken,
			IMAP: g.Provider.IMAP, SMTP: g.Provider.SMTP, SentAppend: g.Provider.SentAppend, AccountID: g.AccountID,
		})
		var tf *accounts.TestFailedError
		switch {
		case errors.Is(err, accounts.ErrAccountMismatch):
			return fail(g.ReturnTo, CodeOAuthAccountMismatch, err)
		case errors.Is(err, accounts.ErrLimitReached):
			return fail(g.ReturnTo, CodeAccountLimit, err)
		case errors.As(err, &tf):
			return fail(g.ReturnTo, CodeAccountTestFailed, err)
		case errors.Is(err, accounts.ErrNotFound):
			return fail(g.ReturnTo, oauth.CodeStateInvalid, err)
		case err != nil:
			return fail(g.ReturnTo, CodeInternal, err)
		}
		return redirect(g.ReturnTo, url.Values{"oauth": {"ok"}, "accountId": {formatID("acc_", acc.ID)}})
	})
}
