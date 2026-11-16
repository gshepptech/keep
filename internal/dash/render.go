package dash

import (
	"strconv"
	"time"

	"github.com/gshepptech/keep/internal/model"
)

// backlogRow pairs an item with whether it opens a new type group in the
// backlog view. items is already grouped and sorted by store.Backlog
// (contiguous per type, in model.TypesOrdered order), so a row's NewGroup is
// true exactly when its Type differs from the previous row's, matching
// cmd/keep/backlog.go's renderBacklog. Nothing here sorts or regroups:
// ranging over model.TypesOrdered and filtering would silently drop any type
// the slice's own order did not anticipate.
type backlogRow struct {
	model.Item
	NewGroup bool
}

// backlogRows builds the backlog view's rows. backlog.html emits a group
// header whenever a row's NewGroup is true.
func backlogRows(items []model.Item) []backlogRow {
	rows := make([]backlogRow, len(items))
	prevType := ""
	for i, it := range items {
		rows[i] = backlogRow{Item: it, NewGroup: i == 0 || it.Type != prevType}
		prevType = it.Type
	}
	return rows
}

// relativeTime renders an RFC3339 instant as the distance from now: "in 4m",
// "12s ago", "expired".
//
// It exists because a lease's expiry is the one field on /who that a person
// reads to make a decision (wait, or pick something else), and
// "2026-08-14T03:56:17Z" does not answer that without arithmetic. Printing
// the raw field is the machine's convenience, not the reader's.
//
// Presentation only, and the third such function in this package after state
// and depthClass. It decides how a value is spelled, never what it means:
// the instant itself comes from Interlock, and a value this cannot parse is
// passed through untouched rather than guessed at or hidden.
func relativeTime(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	d := time.Until(t)
	if d <= 0 {
		return "expired"
	}
	return "in " + shortDuration(d)
}

// shortDuration is the compact spelling: one unit, no decimals, rounded
// down. "in 1m" rather than "in 1 minute 4 seconds", because the reader is
// deciding whether to wait, and the second unit never changes that answer.
func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}

// agoTime renders an RFC3339 instant as how long ago it was: "12s ago".
//
// Separate from relativeTime rather than one function branching on sign,
// because the two spell a past instant differently on purpose. A lease that
// has passed its expiry is "expired": the fact worth reporting is that it
// no longer holds anything, not how long ago that became true. A session
// last seen in the past is "12s ago", where the elapsed time is what
// matters. One function would have to guess which of those a caller meant.
func agoTime(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	d := time.Since(t)
	if d < 0 {
		return "just now"
	}
	return shortDuration(d) + " ago"
}
