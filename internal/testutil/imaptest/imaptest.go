// Package imaptest starts throwaway Dovecot containers for IMAP integration tests.
//
// The configuration lives in testdata/imap (see the README there).
package imaptest

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
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
	// GmailLike has Gmail's folders: [Gmail]/All Mail (\All), [Gmail]/Sent
	// Mail, Drafts, Trash and Spam; no X-GM-EXT-1 (generic mode).
	GmailLike Profile = "gmail-like"
	// OAuth2 adds AUTH=XOAUTH2 and OAUTHBEARER (StartOAuth2).
	OAuth2 Profile = "oauth2"
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
	addr, _ = start(t, profile, nil)
	return addr, User, Pass
}

// StartOAuth2 launches profile OAuth2: Dovecot accepts a bearer token when
// introspectionURL (RFC 7662, POST; e.g. oauthtest.Server.DockerURL() +
// "/introspect") reports it active and logs in the user of its "email".
// The URL must be reachable from the container.
func StartOAuth2(t *testing.T, introspectionURL string) (addr string) {
	t.Helper()
	addr, _ = start(t, OAuth2, strings.NewReplacer("@INTROSPECTION_URL@", introspectionURL))
	return addr
}

func start(t *testing.T, profile Profile, vars *strings.Replacer) (string, testcontainers.Container) {
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
		if vars != nil {
			b = []byte(vars.Replace(string(b)))
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
			// host.docker.internal (a server of the test on the host) exists
			// on Docker Desktop; on Linux it needs the host gateway.
			ExtraHosts: extraHosts(), //nolint:staticcheck // HostConfigModifier would need the moby API module
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
	return fmt.Sprintf("%s:%s", host, port.Port()), ctr
}

func extraHosts() []string {
	if runtime.GOOS == "linux" {
		return []string{"host.docker.internal:host-gateway"}
	}
	return nil
}

// Server is a started Dovecot container (StartServer).
type Server struct {
	Addr string // plaintext IMAP, host:port
	ctr  testcontainers.Container
}

// StartServer is Start that also gives access to the container, for tests
// that need to fill a mailbox faster than APPEND allows (CopyMaildir).
func StartServer(t *testing.T, profile Profile) *Server {
	t.Helper()
	addr, ctr := start(t, profile, nil)
	return &Server{Addr: addr, ctr: ctr}
}

// MailUID is the uid and gid of the mail files in the container (vmail).
const MailUID = 1000

// MaildirPath is the Maildir++ root of a user in the container: INBOX is
// its cur/, another folder X is .X/cur/ (create it over IMAP first).
func MaildirPath(user string) string { return "/tmp/home/" + user + "/mail" }

// CopyTar extracts the tar stream r into dir of the container, keeping the
// uid/gid of the headers (use MailUID). It streams: the archive is never
// held in memory.
func (s *Server) CopyTar(ctx context.Context, dir string, r io.Reader) error {
	cli, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		return err
	}
	defer cli.Close()
	_, err = cli.CopyToContainer(ctx, s.ctr.GetContainerID(), client.CopyToContainerOptions{
		DestinationPath: dir, Content: r, CopyUIDGID: true,
	})
	return err
}
