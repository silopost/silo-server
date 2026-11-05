package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/auth"
	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/store"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
)

const userUsage = `usage: silo-server user <action> [flags]

actions:
  add --email X [--name N] [--admin] [--password-stdin]
        create a user; in mode local the password is read from stdin or
        generated (24 characters, printed once); in mode imap no password is
        stored (the user signs in with the mailbox)
  passwd --email X     set the Silo password from stdin (mode local)
  admin --email X --set=true|false
        grant or revoke admin rights (both modes)
  list                 list users`

// userCmd implements `silo-server user ...`. It needs SILO_DATABASE_URL and
// reads SILO_AUTH_MODE.
func userCmd(ctx context.Context, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if cfg.DatabaseURL == "" {
		return errors.New("SILO_DATABASE_URL is required")
	}
	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	return runUser(ctx, pool, cfg, args, os.Stdin, os.Stdout)
}

func runUser(ctx context.Context, pool *pgxpool.Pool, cfg config.Config, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(userUsage)
	}
	action, args := args[0], args[1:]
	fs := flag.NewFlagSet("user "+action, flag.ContinueOnError)
	email := fs.String("email", "", "login email of the user")
	name := fs.String("name", "", "display name (add)")
	admin := fs.Bool("admin", false, "make the user an admin (add)")
	pwStdin := fs.Bool("password-stdin", false, "read the password from the first line of stdin (add)")
	set := fs.String("set", "", "true or false (admin)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc := auth.New(auth.Options{Pool: pool, Config: cfg})
	q := sqlcgen.New(pool)
	local := cfg.AuthMode == config.AuthModeLocal

	needEmail := func() error {
		if strings.TrimSpace(*email) == "" {
			return fmt.Errorf("user %s: --email is required", action)
		}
		return nil
	}
	findUser := func() (sqlcgen.User, error) {
		u, err := q.GetUserByEmail(ctx, strings.TrimSpace(*email))
		if errors.Is(err, pgx.ErrNoRows) {
			return u, fmt.Errorf("no user with email %q", *email)
		}
		return u, err
	}

	switch action {
	case "add":
		if err := needEmail(); err != nil {
			return err
		}
		var password string
		generated := false
		switch {
		case *pwStdin && !local:
			return errors.New("user add: --password-stdin needs SILO_AUTH_MODE=local (mode imap uses the mailbox password)")
		case *pwStdin:
			p, err := readPassword(stdin)
			if err != nil {
				return err
			}
			password = p
		case local:
			p, err := auth.GeneratePassword()
			if err != nil {
				return err
			}
			password, generated = p, true
		}
		u, err := svc.CreateUser(ctx, auth.NewUser{Email: *email, DisplayName: *name, IsAdmin: *admin, Password: password})
		if err != nil {
			return userErr(err)
		}
		fmt.Fprintf(stdout, "created user %s (id %d, admin=%t)\n", u.Email, u.ID, u.IsAdmin)
		if generated {
			fmt.Fprintf(stdout, "password: %s\n", password)
		}
		return nil

	case "passwd":
		if !local {
			return errors.New("user passwd: Silo passwords exist only with SILO_AUTH_MODE=local")
		}
		if err := needEmail(); err != nil {
			return err
		}
		u, err := findUser()
		if err != nil {
			return err
		}
		p, err := readPassword(stdin)
		if err != nil {
			return err
		}
		if err := svc.SetPassword(ctx, u.ID, p, 0); err != nil {
			return userErr(err)
		}
		fmt.Fprintf(stdout, "password of %s changed; all their sessions were signed out\n", u.Email)
		return nil

	case "admin":
		if err := needEmail(); err != nil {
			return err
		}
		if *set != "true" && *set != "false" {
			return errors.New("user admin: --set=true or --set=false is required")
		}
		u, err := findUser()
		if err != nil {
			return err
		}
		u, err = q.UpdateUserAdmin(ctx, sqlcgen.UpdateUserAdminParams{ID: u.ID, IsAdmin: pgtype.Bool{Bool: *set == "true", Valid: true}})
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s: admin=%t\n", u.Email, u.IsAdmin)
		return nil

	case "list":
		users, err := q.ListUsers(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tEMAIL\tNAME\tADMIN\tDISABLED\tTOTP\tCREATED")
		for _, u := range users {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%t\t%t\t%t\t%s\n", u.ID, u.Email, u.DisplayName, u.IsAdmin,
				u.DisabledAt.Valid, u.TotpEnabledAt.Valid, u.CreatedAt.Time.UTC().Format("2006-01-02 15:04"))
		}
		return tw.Flush()
	}
	return fmt.Errorf("unknown user action %q\n\n%s", action, userUsage)
}

// readPassword takes the first line of r (without the line ending).
func readPassword(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", errors.New("no password on stdin")
	}
	return line, nil
}

func userErr(err error) error {
	switch {
	case errors.Is(err, auth.ErrWeakPassword):
		return fmt.Errorf("the password must have at least %d characters", auth.MinPasswordLen)
	case errors.Is(err, auth.ErrUserExists):
		return errors.New("a user with this email exists")
	case errors.Is(err, auth.ErrInvalidEmail):
		return errors.New("invalid email address")
	}
	return err
}
