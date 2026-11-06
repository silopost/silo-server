package sync_test

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/silopost/silo-server/internal/testutil/imaptest"
)

// appendLimit is the largest mailbox populated over IMAP. Dovecot's Maildir
// gets slower with every APPEND to a large folder (index and uidlist
// updates: about 1000 messages a minute per folder past 15k), so larger
// mailboxes of TestInitialSync are written into the container's Maildir
// with populateMaildir instead.
const appendLimit = 20000

// populateMaildir is populateAged without IMAP: the same messages (ids,
// replies, \Seen, dates) are streamed as a tar of Maildir files into the
// container, generated one at a time (the mailbox is never held in memory).
// The file mtime is the INTERNALDATE; Dovecot assigns the UIDs when it
// first syncs the folders. Folders other than INBOX must exist.
func (mb *mailbox) populateMaildir(ageOf func(i, count int) time.Duration, specs ...spec) map[string]int {
	mb.t.Helper()
	now := time.Now()
	recent := map[string]int{}
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		tw := tar.NewWriter(pw)
		base := 0
		for _, s := range specs {
			root := ""
			if s.folder != "INBOX" {
				root = "." + s.folder + "/"
			}
			// Folders Dovecot has not touched yet (Sent: auto = subscribe)
			// do not exist; directories the tar does not list would be
			// created owned by root.
			var dirs []string
			if root != "" {
				dirs = append(dirs, root)
			}
			dirs = append(dirs, root+"cur/", root+"new/", root+"tmp/")
			for _, d := range dirs {
				if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d, Mode: 0o700, ModTime: now,
					Uid: imaptest.MailUID, Gid: imaptest.MailUID, Format: tar.FormatPAX}); err != nil {
					pw.CloseWithError(err)
					return
				}
			}
			dir := root + "cur/"
			for i := 0; i < s.count; i++ {
				id := base + i + 1
				age := ageOf(i, s.count)
				when := now.Add(-age).Truncate(time.Second)
				reply := 0
				if i%5 == 4 {
					reply = id - 1
				}
				flags := ""
				if i%3 == 0 {
					flags = "S"
				}
				subject := fmt.Sprintf("Message %d", id)
				if reply > 0 {
					subject = fmt.Sprintf("Re: Message %d", reply)
				}
				raw := msg(id, reply, subject, when)
				hdr := &tar.Header{
					Name: fmt.Sprintf("%s%d.M%dP1.silotest:2,%s", dir, when.Unix(), id, flags),
					Mode: 0o600, Size: int64(len(raw)), ModTime: when,
					Uid: imaptest.MailUID, Gid: imaptest.MailUID, Format: tar.FormatPAX,
				}
				if err := tw.WriteHeader(hdr); err != nil {
					pw.CloseWithError(err)
					return
				}
				if _, err := tw.Write(raw); err != nil {
					pw.CloseWithError(err)
					return
				}
				if age < 30*24*time.Hour-time.Hour {
					recent[s.folder]++
				}
			}
			base += s.count
		}
		pw.CloseWithError(tw.Close())
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	err := mb.srv.CopyTar(ctx, imaptest.MaildirPath(mb.user), pr)
	pr.CloseWithError(io.ErrClosedPipe) // stop the writer if the copy failed early
	<-done
	must(mb.t, err)
	return recent
}
