package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/store"
	"github.com/silopost/silo-server/internal/testutil/pgtest"
)

func TestUserCommand(t *testing.T) {
	ctx := context.Background()
	url := pgtest.New(t)
	if err := store.MigrateUp(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	local := config.Config{AuthMode: config.AuthModeLocal}
	imap := config.Config{AuthMode: config.AuthModeIMAP}

	run := func(cfg config.Config, stdin string, args ...string) (string, error) {
		var out bytes.Buffer
		err := runUser(ctx, pool, cfg, args, strings.NewReader(stdin), &out)
		return out.String(), err
	}
	hash := func(email string) (h *string) {
		if err := pool.QueryRow(ctx, `SELECT password_hash FROM users WHERE email = $1`, email).Scan(&h); err != nil {
			t.Fatal(err)
		}
		return h
	}

	// Generated password, printed once.
	out, err := run(local, "", "add", "--email", "root@x.test", "--admin", "--name", "Root")
	if err != nil || !strings.Contains(out, "admin=true") {
		t.Fatalf("add: %q %v", out, err)
	}
	_, pw, ok := strings.Cut(out, "password: ")
	if pw = strings.TrimSpace(pw); !ok || len(pw) != 24 {
		t.Fatalf("generated password %q", pw)
	}
	if h := hash("root@x.test"); h == nil || !strings.HasPrefix(*h, "$argon2id$") {
		t.Fatalf("hash %v", h)
	}

	// Password from stdin; too short is refused; duplicates too.
	if _, err := run(local, "short\n", "add", "--email", "a@x.test", "--password-stdin"); err == nil {
		t.Error("short password accepted")
	}
	if out, err := run(local, "a long password\r\n", "add", "--email", "a@x.test", "--password-stdin"); err != nil || strings.Contains(out, "password:") {
		t.Fatalf("add from stdin: %q %v", out, err)
	}
	if _, err := run(local, "", "add", "--email", "A@x.test"); err == nil {
		t.Error("duplicate email accepted")
	}

	before := *hash("a@x.test")
	if _, err := run(local, "another long one\n", "passwd", "--email", "a@x.test"); err != nil {
		t.Fatal(err)
	}
	if *hash("a@x.test") == before {
		t.Error("passwd did not change the hash")
	}
	if _, err := run(imap, "another long one\n", "passwd", "--email", "a@x.test"); err == nil {
		t.Error("passwd in mode imap accepted")
	}

	// Mode imap: no password is stored; admin can be granted.
	if out, err := run(imap, "", "add", "--email", "mail@x.test"); err != nil || strings.Contains(out, "password:") {
		t.Fatalf("imap add: %q %v", out, err)
	}
	if h := hash("mail@x.test"); h != nil {
		t.Error("imap user got a password hash")
	}
	if out, err := run(imap, "", "admin", "--email", "mail@x.test", "--set=true"); err != nil || !strings.Contains(out, "admin=true") {
		t.Fatalf("admin: %q %v", out, err)
	}
	if _, err := run(imap, "", "admin", "--email", "nobody@x.test", "--set=true"); err == nil {
		t.Error("unknown user accepted")
	}

	out, err = run(imap, "", "list")
	if err != nil || strings.Count(out, "\n") != 4 || !strings.Contains(out, "mail@x.test") {
		t.Fatalf("list: %q %v", out, err)
	}
}
