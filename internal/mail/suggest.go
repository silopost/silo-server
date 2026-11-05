package mail

import (
	"context"
	"strings"
)

// Contact is a suggested address.
type Contact struct {
	Email, Name string
}

// SuggestContacts returns up to limit contacts of the user whose address or
// name (or a word of the name) starts with q, case-insensitive: the most
// written to first, then the most recent (plan 5.1). Contacts are fed by
// ingest (From of received, To/Cc of sent messages) and by sends from Silo.
// It backs /contacts/autocomplete and the contact suggestions of the search
// box.
func SuggestContacts(ctx context.Context, db DB, userID int64, q string, limit int) ([]Contact, error) {
	q = strings.TrimSpace(q)
	out := []Contact{}
	if q == "" || limit <= 0 {
		return out, nil
	}
	like := strings.NewReplacer(`\`, `\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(q)) + "%"
	rows, err := db.Query(ctx, `
SELECT email::text, name FROM contacts
WHERE user_id = $1 AND (lower(email::text) LIKE $2 OR lower(name) LIKE $2 OR lower(name) LIKE '% ' || $2)
ORDER BY sent_count DESC, last_interaction_at DESC NULLS LAST, received_count DESC, email
LIMIT $3`, userID, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c Contact
		if err := rows.Scan(&c.Email, &c.Name); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
