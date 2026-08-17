package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gshepptech/keep/internal/model"
)

// StaleClaim is an item somebody claimed and then went quiet on.
type StaleClaim struct {
	model.Item
	// Silent is how long since anything happened to this item -- not since
	// it was claimed. An agent still writing events every few minutes is
	// working, however long ago it started.
	Silent time.Duration `json:"silent"`
	// LastEvent is when that last thing happened, and what it was.
	LastEvent string `json:"last_event"`
	LastKind  string `json:"last_kind"`
}

// DefaultStaleAfter is when a silent claim starts being worth mentioning.
//
// Two hours rather than twenty minutes: an agent can legitimately spend a
// long time on one item without touching the store, and a threshold that
// fires during ordinary work teaches people to ignore the warning, which is
// worse than not having it. Two hours of total silence is long enough that
// something has almost certainly ended without saying so -- a crashed
// session, a closed terminal, an agent that finished and forgot.
const DefaultStaleAfter = 2 * time.Hour

// StaleClaims finds items sitting in_progress whose last event is older
// than after.
//
// Silence is measured from the item's most recent EVENT, not from
// claimed_at, and that distinction is the whole design. Measuring from the
// claim would call a six-hour piece of work stale while an agent was
// actively writing to it, and would call a claim made one minute ago fresh
// forever if nothing followed. What matters is whether anything is still
// happening.
//
// This reports; it does not release. An automatic unclaim cannot tell a
// dead session from a slow one, and unclaiming live work is a worse failure
// than leaving a dead claim visible -- it would put two agents on one item,
// which is the exact thing keep exists to prevent. The decision stays
// with whoever can look at the terminal.
func (s *Store) StaleClaims(ctx context.Context, after time.Duration) ([]StaleClaim, error) {
	if after <= 0 {
		after = DefaultStaleAfter
	}
	cutoff := nowFunc()
	parsed, err := time.Parse(time.RFC3339Nano, cutoff)
	if err != nil {
		return nil, fmt.Errorf("StaleClaims: unreadable clock: %w", err)
	}
	threshold := parsed.Add(-after).Format(time.RFC3339Nano)

	// The last event per item, joined back to the item. COALESCE covers an
	// item with no events at all, which cannot happen through the store's
	// own writes -- every mutation writes one -- but a hand-edited database
	// should degrade to "stale" rather than vanish from the report.
	query := `SELECT ` + prefixedItemColumns("i") + `,
	       COALESCE(MAX(e.at), i.updated_at) AS last_at,
	       COALESCE((SELECT kind FROM events WHERE item_id = i.id ORDER BY at DESC, id DESC LIMIT 1), 'unknown') AS last_kind
	FROM items i LEFT JOIN events e ON e.item_id = i.id
	WHERE i.status = 'in_progress'
	GROUP BY i.id
	HAVING last_at < ?
	ORDER BY last_at ASC`

	rows, err := s.db.QueryContext(ctx, query, threshold)
	if err != nil {
		return nil, fmt.Errorf("StaleClaims: %w", err)
	}
	defer rows.Close()

	var out []StaleClaim
	for rows.Next() {
		var c StaleClaim
		if err := rows.Scan(
			&c.ID, &c.ParentID, &c.Ordinal, &c.Stage, &c.Type, &c.Title,
			&c.Body, &c.Acceptance, &c.Status, &c.Value, &c.Confidence,
			&c.Effort, &c.Score, &c.Why, &c.Repo, &c.Paths,
			&c.ClaimedBySession, &c.ClaimedAt, &c.Assignee, &c.ExternalRef,
			&c.CreatedAt, &c.UpdatedAt, &c.ClosedAt,
			&c.LastEvent, &c.LastKind,
		); err != nil {
			return nil, fmt.Errorf("StaleClaims: %w", err)
		}
		if at, err := time.Parse(time.RFC3339Nano, c.LastEvent); err == nil {
			c.Silent = parsed.Sub(at)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("StaleClaims: %w", err)
	}
	return out, nil
}

// prefixedItemColumns is itemColumns qualified with a table alias, for the
// queries that join items against something else. It is derived from
// itemColumns rather than written out again, so a future migration that
// adds a column cannot leave this list one short.
func prefixedItemColumns(alias string) string {
	cols := strings.Split(itemColumns, ", ")
	for i, c := range cols {
		cols[i] = alias + "." + strings.TrimSpace(c)
	}
	return strings.Join(cols, ", ")
}
