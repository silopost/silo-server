package ops

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/mail"
)

// Undo cancels an operation inside its undo window (plan 3.6): its jobs are
// deleted (their op_messages go with them) and the messages re-derived, so
// the visible state returns to the server state in the same transaction and
// the IMAP server never sees the operation. Once a job of the operation has
// been claimed by a worker it is too late (ErrTooLate): the caller then
// issues the inverse operation. User labels without a keyword were written
// locally and are not part of the jobs; the inverse modify reverts them.
func (s *Service) Undo(ctx context.Context, userID int64, operationID string, origin string) (*Result, error) {
	res := &Result{}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
SELECT j.id, j.account_id, j.locked_until IS NOT NULL AND j.locked_until > now()
FROM jobs j JOIN accounts a ON a.id = j.account_id
WHERE j.group_id = $1::uuid AND j.kind = 'imap_op' AND j.status = 'pending' AND a.user_id = $2
ORDER BY j.account_id FOR UPDATE OF j`, operationID, userID)
		if err != nil {
			return err
		}
		var jobs, accounts []int64
		locked := false
		for rows.Next() {
			var id, acc int64
			var l bool
			if err := rows.Scan(&id, &acc, &l); err != nil {
				rows.Close()
				return err
			}
			jobs = append(jobs, id)
			if len(accounts) == 0 || accounts[len(accounts)-1] != acc {
				accounts = append(accounts, acc)
			}
			locked = locked || l
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(jobs) == 0 {
			return ErrNotFound
		}
		if locked {
			return ErrTooLate
		}
		for _, acc := range accounts {
			if err := mail.LockAccount(ctx, tx, acc); err != nil {
				return err
			}
			r, err := tx.Query(ctx, `SELECT DISTINCT message_id FROM op_messages WHERE job_id = ANY($1) AND account_id = $2`, jobs, acc)
			if err != nil {
				return err
			}
			msgs, err := pgx.CollectRows(r, pgx.RowTo[int64])
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM jobs WHERE id = ANY($1) AND account_id = $2`, jobs, acc); err != nil {
				return err
			}
			threads, err := mail.RefreshMessages(ctx, tx, acc, msgs, origin)
			if err != nil {
				return err
			}
			res.ThreadIDs = append(res.ThreadIDs, threads...)
		}
		res.ThreadIDs = uniq(res.ThreadIDs)
		res.Threads, err = LoadThreads(ctx, tx, userID, res.ThreadIDs)
		return err
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}
