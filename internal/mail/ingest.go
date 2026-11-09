package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/jackc/pgx/v5"
)

// FetchedMessage is the raw data of one message as returned by
// UID FETCH (UID FLAGS INTERNALDATE RFC822.SIZE BODY.PEEK[HEADER]
// BODYSTRUCTURE [BODY.PEEK[TEXT]<0.N>] [EMAILID] [MODSEQ]).
// Only UID, Header and Size are required.
type FetchedMessage struct {
	UID          uint32
	Flags        []string
	InternalDate time.Time
	Size         int64  // RFC822.SIZE
	ModSeq       uint64 // 0 = unknown (no CONDSTORE)
	EmailID      string // OBJECTID EMAILID or X-GM-MSGID; empty if unsupported
	Header       []byte // BODY[HEADER]
	// TextSnippet is an optional leading fragment of BODY[TEXT] (a few hundred
	// bytes); it is decoded with the encoding/charset from BodyStructure and
	// used for the snippet until the full body is downloaded.
	TextSnippet   []byte
	BodyStructure imap.BodyStructure // optional; nil-safe
	// KnownMessageID is set when the location came from our own operation
	// (APPENDUID/COPYUID) and the message is already known (matcher key 1).
	KnownMessageID int64
}

// Outcome tells what IngestBatch did with one message.
type Outcome int

const (
	// OutcomeNew means a new logical message was created.
	OutcomeNew Outcome = iota
	// OutcomeKnown means the (folder, uid) location already existed; flags refreshed.
	OutcomeKnown
	// OutcomeMoved means the message matched an orphaned message; it is back on the server.
	OutcomeMoved
	// OutcomeDuplicate means the message matched a message that already had a location elsewhere.
	OutcomeDuplicate
	// OutcomeAttached means the message matched a message without any location (e.g. our own
	// appended copy known before sync saw it).
	OutcomeAttached
)

// IngestedMessage is the per-message result of IngestBatch.
type IngestedMessage struct {
	UID       uint32
	MessageID int64
	ThreadID  int64
	Outcome   Outcome
}

// IngestResult is returned by IngestBatch, in input order.
type IngestResult struct {
	Messages []IngestedMessage
	// ThreadIDs are the (final) threads touched by the batch.
	ThreadIDs []int64
}

type accountInfo struct {
	id, userID int64
	email      string
}

type folderInfo struct {
	id, accountID, userID int64
	role                  string
}

func loadAccountFolder(ctx context.Context, db DB, accountID, folderID int64) (accountInfo, folderInfo, error) {
	var a accountInfo
	var f folderInfo
	err := db.QueryRow(ctx, `SELECT id, user_id, lower(email::text) FROM accounts WHERE id = $1`, accountID).Scan(&a.id, &a.userID, &a.email)
	if err != nil {
		return a, f, fmt.Errorf("mail: load account %d: %w", accountID, err)
	}
	err = db.QueryRow(ctx, `SELECT id, account_id, user_id, role FROM mailbox_folders WHERE id = $1 AND account_id = $2`, folderID, accountID).
		Scan(&f.id, &f.accountID, &f.userID, &f.role)
	if err != nil {
		return a, f, fmt.Errorf("mail: load folder %d of account %d: %w", folderID, accountID, err)
	}
	return a, f, nil
}

func normFlags(flags []string) []string {
	out := slices.Clone(flags)
	if out == nil {
		out = []string{}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func nullModseq(m uint64) *int64 {
	if m == 0 {
		return nil
	}
	v := int64(m)
	return &v
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func addrJSON(l []Address) []byte {
	if l == nil {
		l = []Address{}
	}
	b, _ := json.Marshal(l)
	return b
}

func displayAddr(a Address) string {
	switch {
	case a.Email == "":
		return ""
	case a.Name == "":
		return a.Email
	}
	return a.Name + " <" + a.Email + ">"
}

func joinAddrs(lists ...[]Address) string {
	var parts []string
	for _, l := range lists {
		for _, a := range l {
			if s := displayAddr(a); s != "" {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, ", ")
}

// IngestBatch stores a batch of messages fetched from one folder, in the
// caller's transaction (steps 1-5 and 8 of plan 3.5.7):
//
//  1. match each message to a known one (existing location, KnownMessageID,
//     EmailID, dedupe_key = Message-ID + ":" + size); a match with an orphaned
//     message is a move, a match with a located message is a duplicate;
//  2. parse the header and BODYSTRUCTURE (a parse failure is recorded in
//     messages.parse_error and never fails the batch);
//  3. thread new messages (plan 4.6);
//  4. upsert the (folder, uid) locations with flags and modseq;
//  5. DeriveSystemLabels, keyword labels, RecomputeThread once per thread,
//     contacts, and the message.new / thread.updated / label.counts /
//     thread.merged events.
//
// It is idempotent: ingesting the same batch again changes nothing but the
// flags and modseq of existing locations. Step 6 (a new message wakes a
// snoozed thread) is snoozeIngest; step 7 (filters) is filterIngest, which
// queues filter_eval for new INBOX messages (the filters package applies the
// actions as operations without undo, plan 3.11).
func IngestBatch(ctx context.Context, db DB, accountID, folderID int64, msgs []FetchedMessage) (*IngestResult, error) {
	res := &IngestResult{}
	if len(msgs) == 0 {
		return res, nil
	}
	acc, folder, err := loadAccountFolder(ctx, db, accountID, folderID)
	if err != nil {
		return nil, err
	}
	if err := EnsureSystemLabels(ctx, db, accountID); err != nil {
		return nil, err
	}
	ev := &eventBuf{userID: acc.userID}
	ts := newThreadState(ev)

	// Locations that already exist for these UIDs.
	uids := make([]int64, len(msgs))
	for i, m := range msgs {
		uids[i] = int64(m.UID)
	}
	type existingLoc struct {
		messageID int64
		dedupeKey string
	}
	existing := map[uint32]existingLoc{}
	rows, err := db.Query(ctx, `
SELECT l.uid, l.message_id, m.dedupe_key FROM message_locations l JOIN messages m ON m.id = l.message_id
WHERE l.folder_id = $1 AND l.uid = ANY($2)`, folderID, uids)
	if err != nil {
		return nil, fmt.Errorf("mail: load existing locations: %w", err)
	}
	for rows.Next() {
		var uid int64
		var e existingLoc
		if err := rows.Scan(&uid, &e.messageID, &e.dedupeKey); err != nil {
			rows.Close()
			return nil, err
		}
		existing[uint32(uid)] = e
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var allIDs, newIDs []int64
	newFrom := map[int64]*ParsedHeader{}
	for _, m := range msgs {
		ph := ParseHeader(m.Header, m.Size)
		if len(m.Header) == 0 {
			ph.ParseError = joinErr(ph.ParseError, "empty header")
		}
		dedupe := ph.MessageID + ":" + strconv.FormatInt(m.Size, 10)

		out := IngestedMessage{UID: m.UID}
		var msgID, threadID int64
		matched := false
		// A copy of a draft saved by Silo belongs to the local draft (plan 3.8).
		draftMsg, err := draftMessage(ctx, db, accountID, ph.SiloDraftID)
		if err != nil {
			return nil, err
		}
		// A copy in Sent of a message sent by Silo (APPENDed by Silo, or
		// saved by the server itself) belongs to the local message (plan 3.9);
		// so does the old copy of its draft in Drafts until it is removed.
		if draftMsg == 0 && !ph.SyntheticID && (folder.role == "sent" || folder.role == "drafts" && ph.SiloDraftID != "") {
			if draftMsg, err = sentMessage(ctx, db, accountID, ph.MessageID); err != nil {
				return nil, err
			}
		}

		if e, ok := existing[m.UID]; ok {
			if e.dedupeKey == dedupe || draftMsg != 0 && e.messageID == draftMsg {
				msgID, matched, out.Outcome = e.messageID, true, OutcomeKnown
			} else if err := dropLocation(ctx, db, folderID, int64(m.UID), e.messageID, ts); err != nil {
				// The UID now holds a different message (UIDVALIDITY reuse).
				return nil, err
			}
		}
		if !matched {
			if m.KnownMessageID == 0 {
				m.KnownMessageID = draftMsg
			}
			msgID, matched, out.Outcome, err = matchMessage(ctx, db, accountID, m, dedupe)
			if err != nil {
				return nil, err
			}
		}
		if matched {
			if err := db.QueryRow(ctx, `SELECT thread_id FROM messages WHERE id = $1`, msgID).Scan(&threadID); err != nil {
				return nil, fmt.Errorf("mail: load matched message: %w", err)
			}
			if out.Outcome == OutcomeMoved {
				if _, err := db.Exec(ctx, `UPDATE messages SET orphaned_at = NULL WHERE id = $1`, msgID); err != nil {
					return nil, fmt.Errorf("mail: clear orphaned_at: %w", err)
				}
			}
			if m.EmailID != "" {
				if _, err := db.Exec(ctx, `UPDATE messages SET email_id = $2 WHERE id = $1 AND email_id IS NULL`, msgID, m.EmailID); err != nil {
					return nil, fmt.Errorf("mail: set email_id: %w", err)
				}
			}
		} else {
			msgID, threadID, err = insertMessage(ctx, db, ts, acc, m, ph, dedupe)
			if err != nil {
				return nil, err
			}
			out.Outcome = OutcomeNew
			newIDs = append(newIDs, msgID)
			newFrom[msgID] = ph
		}

		if _, err := db.Exec(ctx, `
INSERT INTO message_locations (folder_id, uid, message_id, account_id, user_id, flags, modseq, email_id, stale)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, false)
ON CONFLICT (folder_id, uid) DO UPDATE SET
  message_id = EXCLUDED.message_id, flags = EXCLUDED.flags,
  modseq = COALESCE(EXCLUDED.modseq, message_locations.modseq),
  email_id = COALESCE(EXCLUDED.email_id, message_locations.email_id), stale = false`,
			folderID, int64(m.UID), msgID, accountID, acc.userID, normFlags(m.Flags), nullModseq(m.ModSeq), nullStr(m.EmailID)); err != nil {
			return nil, fmt.Errorf("mail: upsert location %d/%d: %w", folderID, m.UID, err)
		}
		out.MessageID, out.ThreadID = msgID, threadID
		ts.touched[threadID] = true
		allIDs = append(allIDs, msgID)
		res.Messages = append(res.Messages, out)
	}

	// Labels, aggregates, contacts and events for the whole batch.
	allIDs = append(allIDs, ts.dropped...)
	allIDs = uniqueSorted(allIDs)
	if err := DeriveSystemLabelsBatch(ctx, db, allIDs); err != nil {
		return nil, err
	}
	if len(newIDs) > 0 {
		if err := refreshSearch(ctx, db, newIDs); err != nil {
			return nil, err
		}
		if err := upsertContacts(ctx, db, acc, folder, newFrom); err != nil {
			return nil, err
		}
	}
	// Moved/known messages may sit in threads that were merged meanwhile.
	threads, err := threadsOfMessages(ctx, db, allIDs)
	if err != nil {
		return nil, err
	}
	for id := range ts.touched {
		if !slices.Contains(threads, id) && threadExists(ctx, db, id) {
			threads = append(threads, id)
		}
	}
	threads = uniqueSorted(threads)

	// Step 7 before step 6: messages handed to filters wake a snoozed thread
	// only if the filters leave them in INBOX (filter_eval does it).
	filtered, err := filterIngest(ctx, db, accountID, folder.role, newIDs)
	if err != nil {
		return nil, err
	}
	if err := snoozeIngest(ctx, db, folder.role, withoutIDs(newIDs, filtered), threads); err != nil {
		return nil, err
	}
	if len(newIDs) > 0 {
		if err := emitMessageNew(ctx, db, ev, accountID, newIDs, res); err != nil {
			return nil, err
		}
	}
	before, err := labelsOfThreads(ctx, db, threads)
	if err != nil {
		return nil, err
	}
	for l := range ts.labelsBefore {
		before = append(before, l)
	}
	if err := finishThreads(ctx, db, ev, accountID, threads, before); err != nil {
		return nil, err
	}
	if err := ev.flush(ctx, db); err != nil {
		return nil, err
	}
	// Report final thread ids (a later message of the batch may have merged
	// the thread an earlier one was placed in).
	for i := range res.Messages {
		if err := db.QueryRow(ctx, `SELECT thread_id FROM messages WHERE id = $1`, res.Messages[i].MessageID).Scan(&res.Messages[i].ThreadID); err != nil {
			return nil, err
		}
	}
	res.ThreadIDs = threads
	return res, nil
}

func joinErr(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

func threadExists(ctx context.Context, db DB, id int64) bool {
	var one int
	return db.QueryRow(ctx, `SELECT 1 FROM threads WHERE id = $1`, id).Scan(&one) == nil
}

func threadsOfMessages(ctx context.Context, db DB, msgIDs []int64) ([]int64, error) {
	rows, err := db.Query(ctx, `SELECT DISTINCT thread_id FROM messages WHERE id = ANY($1)`, msgIDs)
	if err != nil {
		return nil, fmt.Errorf("mail: load threads of messages: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// matchMessage is the Matcher of plan 3.5.6 (the existing-location key was
// handled by the caller): KnownMessageID (also the local draft named by
// X-Silo-Draft-Id), EmailID, dedupe_key.
func matchMessage(ctx context.Context, db DB, accountID int64, m FetchedMessage, dedupe string) (int64, bool, Outcome, error) {
	var id int64
	var orphaned, located bool
	scan := func(q string, args ...any) (bool, error) {
		err := db.QueryRow(ctx, q, args...).Scan(&id, &orphaned, &located)
		if err == pgx.ErrNoRows {
			return false, nil
		}
		return err == nil, err
	}
	const sel = `SELECT m.id, m.orphaned_at IS NOT NULL, EXISTS (SELECT 1 FROM message_locations l WHERE l.message_id = m.id) FROM messages m WHERE m.account_id = $1 AND `
	found := false
	var err error
	if m.KnownMessageID != 0 {
		found, err = scan(sel+`m.id = $2`, accountID, m.KnownMessageID)
	}
	if err == nil && !found && m.EmailID != "" {
		found, err = scan(sel+`m.email_id = $2 ORDER BY m.id LIMIT 1`, accountID, m.EmailID)
	}
	if err == nil && !found {
		found, err = scan(sel+`m.dedupe_key = $2`, accountID, dedupe)
	}
	if err != nil {
		return 0, false, 0, fmt.Errorf("mail: match message: %w", err)
	}
	if !found {
		return 0, false, 0, nil
	}
	switch {
	case orphaned:
		return id, true, OutcomeMoved, nil
	case located:
		return id, true, OutcomeDuplicate, nil
	}
	return id, true, OutcomeAttached, nil
}

// draftMessage returns the local draft whose X-Silo-Draft-Id is id (0 if none).
func draftMessage(ctx context.Context, db DB, accountID int64, id string) (int64, error) {
	if id == "" {
		return 0, nil
	}
	var msgID int64
	err := db.QueryRow(ctx, `SELECT message_id FROM drafts WHERE account_id = $1 AND silo_draft_id = $2`, accountID, id).Scan(&msgID)
	if err == pgx.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("mail: match draft: %w", err)
	}
	return msgID, nil
}

// sentMessage returns the local message sent by Silo with this Message-ID
// (0 if none).
func sentMessage(ctx context.Context, db DB, accountID int64, msgIDHdr string) (int64, error) {
	var msgID int64
	err := db.QueryRow(ctx, `
SELECT m.id FROM messages m JOIN outbound o ON o.message_id = m.id
WHERE m.account_id = $1 AND m.message_id_hdr = $2 AND m.body_state = 'local' AND o.status = 'sent'
ORDER BY m.id LIMIT 1`, accountID, msgIDHdr).Scan(&msgID)
	if err == pgx.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("mail: match sent message: %w", err)
	}
	return msgID, nil
}

// dropLocation removes one location (used when a UID is reused by another
// message) and orphans the message if it has no other location.
func dropLocation(ctx context.Context, db DB, folderID, uid, messageID int64, ts *threadState) error {
	if _, err := db.Exec(ctx, `DELETE FROM message_locations WHERE folder_id = $1 AND uid = $2`, folderID, uid); err != nil {
		return fmt.Errorf("mail: drop location: %w", err)
	}
	if _, err := db.Exec(ctx, `
UPDATE messages SET orphaned_at = now()
WHERE id = $1 AND orphaned_at IS NULL AND NOT EXISTS (SELECT 1 FROM message_locations WHERE message_id = $1)
  AND NOT EXISTS (SELECT 1 FROM drafts WHERE message_id = $1)`, messageID); err != nil {
		return fmt.Errorf("mail: orphan message: %w", err)
	}
	ts.dropped = append(ts.dropped, messageID)
	var tid int64
	if err := db.QueryRow(ctx, `SELECT thread_id FROM messages WHERE id = $1`, messageID).Scan(&tid); err == nil {
		ts.touched[tid] = true
	}
	return nil
}

// insertMessage threads and inserts a new message with its references and
// BODYSTRUCTURE attachments.
func insertMessage(ctx context.Context, db DB, ts *threadState, acc accountInfo, m FetchedMessage, ph *ParsedHeader, dedupe string) (msgID, threadID int64, err error) {
	hasList := ph.ListID != ""
	subjNorm, replyPrefix := NormalizeSubject(ph.Subject, hasList)
	internal := m.InternalDate
	if internal.IsZero() {
		if ph.Date != nil {
			internal = *ph.Date
		} else {
			internal = time.Now()
		}
	}
	atts := AttachmentsFromStructure(m.BodyStructure)
	hasAtt := hasRealAttachment(atts)
	if m.BodyStructure == nil && strings.HasPrefix(ph.ContentType, "multipart/mixed") {
		hasAtt = true // best guess without BODYSTRUCTURE
	}
	snippet := ""
	if len(m.TextSnippet) > 0 {
		enc, cs, isHTML, _ := firstTextPart(m.BodyStructure)
		snippet = MakeSnippet(decodeFragment(m.TextSnippet, enc, cs, isHTML))
	}

	threadID, err = placeInThread(ctx, db, ts, threadInput{
		accountID: acc.id, userID: acc.userID, messageID: ph.MessageID, refs: ph.Refs,
		subject: ph.Subject, subjectNorm: subjNorm, replyPrefix: replyPrefix, date: internal,
		addrs: participantAddrs(acc.email, []Address{ph.From}, ph.To, ph.Cc),
	})
	if err != nil {
		return 0, 0, err
	}
	var replyTo []byte
	if len(ph.ReplyTo) > 0 {
		replyTo = addrJSON(ph.ReplyTo)
	}
	headerDate := ph.Date
	err = db.QueryRow(ctx, `
INSERT INTO messages (user_id, account_id, thread_id, message_id_hdr, dedupe_key, email_id, in_reply_to_hdr,
  body_state, size_bytes, internal_date, header_date, from_addr, from_name, from_text, to_addrs, cc_addrs,
  recipients_text, reply_to, subject, subject_norm, reply_prefix, snippet, has_attachments, list_id,
  list_unsubscribe, parse_error)
VALUES ($1,$2,$3,$4,$5,$6,$7,'headers',$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)
RETURNING id`,
		acc.userID, acc.id, threadID, ph.MessageID, dedupe, nullStr(m.EmailID), nullStr(ph.InReplyTo),
		m.Size, internal, headerDate, ph.From.Email, ph.From.Name, displayAddr(ph.From), addrJSON(ph.To), addrJSON(ph.Cc),
		joinAddrs(ph.To, ph.Cc), replyTo, ph.Subject, subjNorm, replyPrefix, snippet, hasAtt, nullStr(ph.ListID),
		nullStr(ph.ListUnsubscribe), nullStr(ph.ParseError)).Scan(&msgID)
	if err != nil {
		return 0, 0, fmt.Errorf("mail: insert message: %w", err)
	}
	if len(ph.Refs) > 0 {
		if _, err := db.Exec(ctx, `
INSERT INTO message_references (message_id, user_id, account_id, ref_message_id_hdr, position)
SELECT $1, $2, $3, r.ref, r.pos FROM unnest($4::text[]) WITH ORDINALITY AS r(ref, pos)`,
			msgID, acc.userID, acc.id, ph.Refs); err != nil {
			return 0, 0, fmt.Errorf("mail: insert references: %w", err)
		}
	}
	for _, a := range atts {
		if _, err := db.Exec(ctx, `
INSERT INTO attachments (message_id, user_id, filename, content_type, size_bytes, content_id, is_inline, part_path)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			msgID, acc.userID, a.Filename, a.ContentType, a.Size, nullStr(a.ContentID), a.Inline, a.PartPath); err != nil {
			return 0, 0, fmt.Errorf("mail: insert attachment: %w", err)
		}
	}
	if err := refreshThreadSubject(ctx, db, threadID); err != nil {
		return 0, 0, err
	}
	ts.touched[threadID] = true
	return msgID, threadID, nil
}

// upsertContacts counts new messages: received_count for the sender of mail
// from others, sent_count for the recipients of mail from the account itself.
func upsertContacts(ctx context.Context, db DB, acc accountInfo, _ folderInfo, msgs map[int64]*ParsedHeader) error {
	type agg struct {
		name           string
		sent, received int
	}
	byEmail := map[string]*agg{}
	get := func(a Address) *agg {
		g := byEmail[a.Email]
		if g == nil {
			g = &agg{}
			byEmail[a.Email] = g
		}
		if g.name == "" {
			g.name = a.Name
		}
		return g
	}
	for _, ph := range msgs {
		if ph.From.Email == "" {
			continue
		}
		if ph.From.Email == acc.email {
			for _, a := range append(slices.Clone(ph.To), ph.Cc...) {
				if a.Email != "" && a.Email != acc.email {
					get(a).sent++
				}
			}
		} else {
			get(ph.From).received++
		}
	}
	if len(byEmail) == 0 {
		return nil
	}
	var emails, names []string
	var sent, received []int32
	for e, g := range byEmail {
		emails, names, sent, received = append(emails, e), append(names, g.name), append(sent, int32(g.sent)), append(received, int32(g.received))
	}
	_, err := db.Exec(ctx, `
INSERT INTO contacts (user_id, email, name, sent_count, received_count, last_interaction_at)
SELECT $1, c.email, c.name, c.sent, c.received, now()
FROM unnest($2::text[], $3::text[], $4::int[], $5::int[]) AS c(email, name, sent, received)
ON CONFLICT (user_id, email) DO UPDATE SET
  sent_count = contacts.sent_count + EXCLUDED.sent_count,
  received_count = contacts.received_count + EXCLUDED.received_count,
  name = CASE WHEN contacts.name = '' THEN EXCLUDED.name ELSE contacts.name END,
  last_interaction_at = now()`, acc.userID, emails, names, sent, received)
	if err != nil {
		return fmt.Errorf("mail: upsert contacts: %w", err)
	}
	return nil
}

// emitMessageNew writes message.new for each newly created message.
func emitMessageNew(ctx context.Context, db DB, ev *eventBuf, accountID int64, newIDs []int64, _ *IngestResult) error {
	rows, err := db.Query(ctx, `
SELECT m.id, m.thread_id, m.from_addr::text, m.from_name, m.subject, m.snippet,
       COALESCE((SELECT array_agg(ml.label_id ORDER BY ml.label_id) FROM message_labels ml WHERE ml.message_id = m.id), '{}')
FROM messages m WHERE m.id = ANY($1) ORDER BY m.id`, newIDs)
	if err != nil {
		return fmt.Errorf("mail: load message.new: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, tid int64
		var from, name, subject, snippet string
		var labels []int64
		if err := rows.Scan(&id, &tid, &from, &name, &subject, &snippet, &labels); err != nil {
			return err
		}
		ev.add(EventMessageNew, map[string]any{
			"accountId": accountID, "threadId": tid, "messageId": id, "labelIds": labels,
			"from": map[string]string{"name": name, "email": from}, "subject": subject, "snippet": snippet,
		})
	}
	return rows.Err()
}
