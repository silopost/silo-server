package filters

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/mailview"
	"github.com/silopost/silo-server/internal/ops"
	"github.com/silopost/silo-server/internal/search"
	"github.com/silopost/silo-server/internal/worker"
)

// NeedsBody reports whether the query has a term that looks at the body (a
// word or phrase without a key), so that a filter with it waits for the
// body of a new message (plan 3.11).
func NeedsBody(a search.AST) bool {
	found := false
	search.Walk(a.Root, func(n search.Node) {
		if t, ok := n.(search.Term); ok && t.Key == "" {
			found = true
		}
	})
	return found
}

type compiled struct {
	f   Filter
	ast search.AST
}

// Match evaluates filters (in position order) on one message and returns
// the merged actions of those that match and the forward addresses (in
// filter order, without duplicates).
func match(m search.Matcher, fs []compiled, v search.MessageView) (Actions, []string, bool) {
	var acts Actions
	var fwd []string
	matched := false
	for _, c := range fs {
		if !m.Match(c.ast, v) {
			continue
		}
		matched = true
		acts = acts.merge(c.f.Actions)
		if to := c.f.Actions.ForwardTo; to != "" && !slices.Contains(fwd, to) {
			fwd = append(fwd, to)
		}
	}
	return acts, fwd, matched
}

// liveLabels drops labels that are no longer user labels of the account
// (deleted after the filter was saved).
func (s *Service) liveLabels(ctx context.Context, accountID int64, a Actions) (Actions, error) {
	if len(a.AddLabelIDs) == 0 {
		return a, nil
	}
	rows, err := s.o.Pool.Query(ctx, `SELECT id FROM labels WHERE id = ANY($1) AND account_id = $2 AND type = 'user' ORDER BY id`, a.AddLabelIDs, accountID)
	if err != nil {
		return a, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return a, err
		}
		ids = append(ids, id)
	}
	a.AddLabelIDs = ids
	return a, rows.Err()
}

// runEval is filter_eval (plan 3.5.7 step 7): the enabled filters of the
// account on new INBOX messages. When a filter looks at the body (or
// forwards) and a body is not downloaded yet, it is fetched with priority
// and waited for until QueuedAt + BodyWait; then the filters run on what is
// there. The actions of all matching filters are applied together, one
// operation per set of actions, without the undo window. Messages the
// filters leave in INBOX wake their snoozed thread (step 6, deferred to here
// so that a filtered-out message does not wake it).
func (s *Service) runEval(ctx context.Context, job worker.Job) error {
	var p mail.FilterEvalPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return worker.Permanent(err)
	}
	var userID int64
	var self string
	if err := s.o.Pool.QueryRow(ctx, `SELECT user_id, lower(email::text) FROM accounts WHERE id = $1`, p.AccountID).Scan(&userID, &self); err != nil {
		return nil // account removed
	}
	stored, err := list(ctx, s.o.Pool, userID, p.AccountID, true)
	if err != nil {
		return err
	}
	var fs []compiled
	needBody := false
	for _, f := range stored {
		a, err := ParseCriteria(f.Criteria)
		if err != nil {
			s.log.Warn("filter with invalid criteria skipped", "filter", f.ID, "err", err)
			continue
		}
		fs = append(fs, compiled{f: f, ast: a})
		needBody = needBody || NeedsBody(a) || f.Actions.ForwardTo != ""
	}
	views, err := mail.LoadMessageViews(ctx, s.o.Pool, p.MessageIDs)
	if err != nil {
		return err
	}
	if len(fs) > 0 && needBody && s.o.Bodies != nil {
		if s.waitBodies(ctx, p, views) {
			if views, err = mail.LoadMessageViews(ctx, s.o.Pool, p.MessageIDs); err != nil {
				return err
			}
		}
	}
	scope, err := mailview.LoadSearchScope(ctx, s.o.Pool, userID, p.AccountID)
	if err != nil {
		return err
	}
	m := search.Matcher{Location: scope.Location, Now: time.Now()}
	type group struct {
		acts Actions
		ids  []int64
	}
	groups := map[string]*group{}
	var keys []string
	var stay []int64
	type forward struct {
		id int64
		to string
	}
	var forwards []forward
	for _, v := range views {
		acts, fwd, matched := match(m, fs, v.View)
		if !acts.Archive && !acts.Trash {
			stay = append(stay, v.ID)
		}
		if !matched {
			continue
		}
		if v.FromEmail != self { // never forward the account's own mail (loops)
			for _, to := range fwd {
				forwards = append(forwards, forward{v.ID, to})
			}
		}
		acts.ForwardTo = ""
		if acts.Empty() {
			continue
		}
		raw, _ := json.Marshal(acts)
		k := string(raw)
		if groups[k] == nil {
			groups[k] = &group{acts: acts}
			keys = append(keys, k)
		}
		groups[k].ids = append(groups[k].ids, v.ID)
	}
	for _, k := range keys {
		g := groups[k]
		acts, err := s.liveLabels(ctx, p.AccountID, g.acts)
		if err != nil {
			return err
		}
		add, remove := acts.labelChange()
		if len(add)+len(remove) == 0 {
			continue
		}
		_, err = s.o.Ops.ModifyLabels(ctx, ops.Modify{UserID: userID, MessageIDs: g.ids, Add: add, Remove: remove, Immediate: true})
		if err != nil && !errors.Is(err, ops.ErrNotFound) {
			return err
		}
	}
	if err := mail.WakeSnoozedBy(ctx, s.o.Pool, stay); err != nil {
		return err
	}
	// Forwards last and never retried: a copy that left must not leave twice.
	for _, f := range forwards {
		if s.o.Forward == nil {
			break
		}
		if _, err := s.o.Forward.Forward(ctx, userID, f.id, f.to, ""); err != nil {
			s.log.Warn("filter forward failed", "message", f.id, "to", f.to, "err", err)
		}
	}
	return nil
}

// waitBodies gets the bodies of messages that have none yet before the
// deadline of the job (QueuedAt + BodyWait). The sync engine downloads the
// bodies of new INBOX messages at once, so a body may arrive by itself; a
// message still without one is fetched with priority (BodyFetcher). It
// reports whether any body was missing.
func (s *Service) waitBodies(ctx context.Context, p mail.FilterEvalPayload, views []mail.MessageView) bool {
	deadline := p.QueuedAt.Add(s.o.BodyWait)
	missing := false
	for _, v := range views {
		if v.BodyState != "headers" && v.BodyState != "deferred" {
			continue
		}
		missing = true
		var state string
		if err := s.o.Pool.QueryRow(ctx, `SELECT body_state FROM messages WHERE id = $1`, v.ID).Scan(&state); err != nil || state != v.BodyState {
			continue // downloaded meanwhile (or gone)
		}
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		cctx, cancel := context.WithTimeout(ctx, left)
		err := s.o.Bodies.FetchBody(cctx, p.AccountID, v.ID)
		cancel()
		if err != nil {
			s.log.Debug("filter: body not fetched, evaluating on the headers", "message", v.ID, "err", err)
		}
	}
	return missing
}
