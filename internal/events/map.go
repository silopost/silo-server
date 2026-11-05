package events

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
)

func b36(prefix string, id int64) string { return prefix + strconv.FormatInt(id, 36) }

// Payload keys that hold database ids, by the prefix of their API id.
var (
	idKeys    = map[string]string{"accountId": "acc_", "threadId": "t_", "messageId": "m_", "intoId": "t_", "draftId": "d_", "outboundId": "ob_", "filterId": "f_"}
	idListKey = map[string]string{"threadIds": "t_", "fromIds": "t_"}
)

func asInt(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
}

// mapPayload translates the database ids of an event payload to API ids:
// accountId -> acc_, threadId/intoId/threadIds/fromIds -> t_, messageId ->
// m_, draftId -> d_, outboundId -> ob_, filterId -> f_, label ids -> the system key (INBOX, ...) or l_. thread.updated also
// gets the pseudo label UNREAD in labelIds when unreadCount > 0, as the
// thread shape of the REST API has it.
func (h *Hub) mapPayload(ctx context.Context, typ string, raw []byte) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	// Collect label ids first so that one query resolves them all.
	var want []int64
	collect := func(v any) {
		if id, ok := asInt(v); ok {
			want = append(want, id)
		}
	}
	if l, ok := m["labelIds"].([]any); ok {
		for _, v := range l {
			collect(v)
		}
	}
	collect(m["labelId"])
	if l, ok := m["labels"].([]any); ok {
		for _, v := range l {
			if o, ok := v.(map[string]any); ok {
				collect(o["id"])
			}
		}
	}
	if err := h.resolveLabels(ctx, want); err != nil {
		return nil, err
	}
	label := func(v any) any {
		if id, ok := asInt(v); ok {
			return h.labelID(id)
		}
		return v
	}
	for k, v := range m {
		switch {
		case idKeys[k] != "":
			if id, ok := asInt(v); ok {
				m[k] = b36(idKeys[k], id)
			}
		case idListKey[k] != "":
			if l, ok := v.([]any); ok {
				for i, x := range l {
					if id, ok := asInt(x); ok {
						l[i] = b36(idListKey[k], id)
					}
				}
			}
		case k == "labelIds":
			if l, ok := v.([]any); ok {
				out := make([]any, 0, len(l)+1)
				for _, x := range l {
					out = append(out, label(x))
				}
				if n, ok := asInt(m["unreadCount"]); ok && n > 0 && typ == "thread.updated" {
					out = append(out, "UNREAD")
				}
				m[k] = out
			}
		case k == "labelId":
			m[k] = label(v)
		case k == "labels":
			if l, ok := v.([]any); ok {
				for _, x := range l {
					if o, ok := x.(map[string]any); ok {
						o["id"] = label(o["id"])
					}
				}
			}
		}
	}
	return json.Marshal(m)
}

// labelID returns the API id of a label resolved by resolveLabels.
func (h *Hub) labelID(id int64) string {
	if v, ok := h.labels.Load(id); ok {
		return v.(string)
	}
	return b36("l_", id)
}

// resolveLabels loads the system keys of labels not seen before. System
// labels never change their key, so the cache needs no invalidation; labels
// that do not exist (deleted) fall back to their l_ id.
func (h *Hub) resolveLabels(ctx context.Context, ids []int64) error {
	var missing []int64
	for _, id := range ids {
		if _, ok := h.labels.Load(id); !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	rows, err := h.opts.Pool.Query(ctx, `SELECT id, system_key FROM labels WHERE id = ANY($1)`, missing)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var key *string
		if err := rows.Scan(&id, &key); err != nil {
			return err
		}
		if key != nil {
			h.labels.Store(id, *key)
		} else {
			h.labels.Store(id, b36("l_", id))
		}
	}
	return rows.Err()
}
