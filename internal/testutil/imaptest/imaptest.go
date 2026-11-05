// Package imaptest starts throwaway Dovecot containers for IMAP integration tests.
//
// The configuration lives in testdata/imap (see the README there).
package imaptest

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image is the pinned Dovecot image used by all profiles.
const Image = "dovecot/dovecot:2.4.5"

// Credentials of the test account every profile provides.
const (
	User = "silo"
	Pass = "silopass"
)

// Credentials of a second mailbox on the same server (another Silo user, or
// a second account of one user).
const (
	User2 = "silo2"
	Pass2 = "silopass2"
)

// Profile selects which IMAP extensions the server advertises.
type Profile string

const (
	// Full is the Dovecot default: CONDSTORE QRESYNC MOVE UIDPLUS IDLE ...
	Full Profile = "full"
	// NoCondstore hides CONDSTORE and QRESYNC (fallback sync path).
	NoCondstore Profile = "no-condstore"
	// NoMove hides MOVE and UIDPLUS (COPY + STORE + EXPUNGE fallback).
	NoMove Profile = "no-move"
)

const imapPort = "31143/tcp"

//go:embed testdata/imap/dovecot.conf testdata/imap/users testdata/imap/profile-*.conf
var files embed.FS

// Start launches a Dovecot container with the given profile and returns the
// plaintext IMAP address (host:port) and the test credentials.
//
// Tests are skipped with -short and when Docker is unavailable, unless
// SILO_REQUIRE_DOCKER=true (set in CI), in which case they fail.
func Start(t *testing.T, profile Profile) (addr, user, pass string) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped with -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	conf := map[string]string{
		"/etc/dovecot/dovecot.conf": "testdata/imap/dovecot.conf",
		"/etc/dovecot/users":        "testdata/imap/users",
		"/etc/dovecot/profile.conf": "testdata/imap/profile-" + string(profile) + ".conf",
	}
	var cfiles []testcontainers.ContainerFile
	for target, src := range conf {
		b, err := files.ReadFile(src)
		if err != nil {
			t.Fatalf("unknown imaptest profile %q: %v", profile, err)
		}
		cfiles = append(cfiles, testcontainers.ContainerFile{
			Reader:            bytes.NewReader(b),
			ContainerFilePath: target,
			FileMode:          0o644,
		})
	}

	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        Image,
			ExposedPorts: []string{imapPort},
			Files:        cfiles,
			WaitingFor:   wait.ForListeningPort(imapPort).WithStartupTimeout(time.Minute),
		},
		Started: true,
	})
	if ctr != nil {
		testcontainers.CleanupContainer(t, ctr)
	}
	if err != nil {
		if os.Getenv("SILO_REQUIRE_DOCKER") == "true" {
			t.Fatalf("start dovecot container: %v", err)
		}
		t.Skipf("docker unavailable: %v", err)
	}

	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, imapPort)
	if err != nil {
		t.Fatalf("mapped port: %v", err)
	}
	return fmt.Sprintf("%s:%s", host, port.Port()), User, Pass
}
