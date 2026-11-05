package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"time"

	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/mail"
)

// item is one message of an operation with the folder it is taken from.
type item struct {
	messageID int64
	expect    *int64
}

// canonical sorts the flag lists so equal operations have equal JSON.
func canonical(p mail.OpPayload) mail.OpPayload {
	p.Add, p.Remove = slices.Clone(p.Add), slices.Clone(p.Remove)
	slices.Sort(p.Add)
	slices.Sort(p.Remove)
	p.Add, p.Remove = slices.Compact(p.Add), slices.Compact(p.Remove)
	return p
}

func payloadJSON(p mail.OpPayload) []byte {
	b, _ := json.Marshal(p)
	return b
}

// enqueue adds an operation on messages of one account inside tx (plan 3.6),
// with coalescing against pending jobs that no worker has claimed yet:
//
//   - a move cancels earlier unclaimed moves of the same message (the new one
//     starts from the folder of the earliest, the message's server folder);
//     when that folder is the new target, nothing is left to do;
//   - flags cancel the exact inverse flags change of the same message;
//   - flags join an unclaimed job with the same flags change (10 x "mark
//     read" = one UID STORE); moves join only jobs of the same operation,
//     so that undo by group_id stays exact.
//
// The new job runs at greatest(now + delay, run_at of the pending jobs
// touching the same messages). It returns the job id (0 if none was needed)
// and the messages whose visible state may have changed.
func enqueue(ctx context.Context, tx pgx.Tx, accountID, userID int64, group string, p mail.OpPayload, items []item, delay time.Duration, folderRole map[int64]string) (int64, []int64, error) {
	p = canonical(p)
	if len(items) == 0 {
		return 0, nil, nil
	}
	ids := make([]int64, len(items))
	for i, it := range items {
		ids[i] = it.messageID
	}
	touched := slices.Clone(ids)
	keep := map[int64]*int64{}
	for _, it := range items {
		keep[it.messageID] = it.expect
	}

	var emptied []int64
	drop := func(jobID, msgID int64) error {
		_, err := tx.Exec(ctx, `DELETE FROM op_messages WHERE job_id = $1 AND message_id = $2`, jobID, msgID)
		emptied = append(emptied, jobID)
		return err
	}
	unclaimed := `j.kind = 'imap_op' AND j.status = 'pending' AND j.account_id = $1 AND (j.locked_until IS NULL OR j.locked_until < now())`

	switch p.Op {
	case mail.OpMove:
		rows, err := tx.Query(ctx, `
SELECT om.job_id, om.message_id, om.expect_folder_id FROM op_messages om JOIN jobs j ON j.id = om.job_id
WHERE `+unclaimed+` AND j.payload->>'op' = 'move' AND om.message_id = ANY($2)
ORDER BY j.run_at, j.id`, accountID, ids)
		if err != nil {
			return 0, nil, err
		}
		type prev struct {
			job, msg int64
			expect   *int64
		}
		var olds []prev
		for rows.Next() {
			var o prev
			if err := rows.Scan(&o.job, &o.msg, &o.expect); err != nil {
				rows.Close()
				return 0, nil, err
			}
			olds = append(olds, o)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, nil, err
		}
		seen := map[int64]bool{}
		for _, o := range olds {
			if err := drop(o.job, o.msg); err != nil {
				return 0, nil, err
			}
			if seen[o.msg] {
				continue
			}
			seen[o.msg] = true
			keep[o.msg] = o.expect // the earliest move knows where it really is
			if o.expect != nil && (p.ToFolderID == *o.expect || p.ToFolderID == 0 && folderRole[*o.expect] == p.ToRole) {
				delete(keep, o.msg) // moved back where it is on the server
			}
		}
	case mail.OpFlags:
		inv := payloadJSON(canonical(mail.OpPayload{Op: mail.OpFlags, Add: p.Remove, Remove: p.Add}))
		rows, err := tx.Query(ctx, `
SELECT om.job_id, om.message_id FROM op_messages om JOIN jobs j ON j.id = om.job_id
WHERE `+unclaimed+` AND j.payload = $3::jsonb AND om.message_id = ANY($2)`, accountID, ids, inv)
		if err != nil {
			return 0, nil, err
		}
		var pairs [][2]int64
		for rows.Next() {
			var a, b int64
			if err := rows.Scan(&a, &b); err != nil {
				rows.Close()
				return 0, nil, err
			}
			pairs = append(pairs, [2]int64{a, b})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, nil, err
		}
		for _, pr := range pairs {
			if err := drop(pr[0], pr[1]); err != nil {
				return 0, nil, err
			}
			delete(keep, pr[1])
		}
	}
	if len(emptied) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM jobs j WHERE j.id = ANY($1) AND NOT EXISTS (SELECT 1 FROM op_messages om WHERE om.job_id = j.id)`, emptied); err != nil {
			return 0, nil, err
		}
	}
	if len(keep) == 0 {
		return 0, touched, nil
	}
	msgIDs := make([]int64, 0, len(keep))
	expects := make([]*int64, 0, len(keep))
	for id, e := range keep {
		msgIDs, expects = append(msgIDs, id), append(expects, e)
	}

	// Join an equal unclaimed job.
	var jobID int64
	q := `SELECT j.id FROM jobs j WHERE ` + unclaimed + ` AND j.payload = $2::jsonb`
	args := []any{accountID, payloadJSON(p)}
	if p.Op != mail.OpFlags {
		q += ` AND j.group_id = $3::uuid`
		args = append(args, group)
	}
	err := tx.QueryRow(ctx, q+` ORDER BY j.run_at DESC, j.id DESC LIMIT 1 FOR UPDATE`, args...).Scan(&jobID)
	switch {
	case err == pgx.ErrNoRows:
		err = tx.QueryRow(ctx, `
INSERT INTO jobs (kind, account_id, group_id, payload, run_at, max_attempts)
SELECT 'imap_op', $1, $2::uuid, $3::jsonb,
       GREATEST(now() + make_interval(secs => $4),
                COALESCE((SELECT max(j.run_at) FROM jobs j JOIN op_messages om ON om.job_id = j.id
                          WHERE j.kind = 'imap_op' AND j.status = 'pending' AND om.message_id = ANY($5)), now())),
       $6
RETURNING id`, accountID, group, payloadJSON(p), delay.Seconds(), msgIDs, MaxAttempts).Scan(&jobID)
		if err != nil {
			return 0, nil, fmt.Errorf("ops: insert job: %w", err)
		}
	case err != nil:
		return 0, nil, err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO op_messages (job_id, message_id, account_id, user_id, expect_folder_id)
SELECT $1, m, $2, $3, e FROM unnest($4::bigint[], $5::bigint[]) AS v(m, e)
ON CONFLICT (job_id, message_id) DO NOTHING`, jobID, accountID, userID, msgIDs, expects); err != nil {
		return 0, nil, fmt.Errorf("ops: insert op messages: %w", err)
	}
	return jobID, touched, nil
}
