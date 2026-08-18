package model

import "time"

// TimeFormat is the layout every timestamp keep writes uses. It is
// deliberately NOT time.RFC3339Nano: RFC3339Nano trims trailing zeros from the
// fractional second, which makes its output variable-width, and variable-width
// timestamps do not sort lexically in the order they occurred --
// "2026-01-01T00:00:00Z" sorts AFTER "2026-01-01T00:00:00.5Z" because '.'
// (0x2E) precedes 'Z' (0x5A). The ready view orders by created_at ASC on a
// TEXT column (see internal/store/schema.go), so a variable-width format would
// silently mis-order the queue's tiebreak. The .000000000 verb keeps trailing
// zeros, giving a fixed 30-character UTC string that sorts chronologically.
const TimeFormat = "2006-01-02T15:04:05.000000000Z"

// Now returns the current instant formatted with TimeFormat, in UTC. It is
// the one place callers should derive "now" for a keep timestamp, so no
// caller re-derives TimeFormat's usage independently.
func Now() string {
	return FormatTime(time.Now())
}

// FormatTime formats t with TimeFormat, in UTC. Tests and callers that need a
// specific instant (rather than "now") use this directly.
func FormatTime(t time.Time) string {
	return t.UTC().Format(TimeFormat)
}

// Item mirrors the items table (internal/store/schema.go, schemaDDL) one
// field per column, in column order. Pointer types are used for exactly the
// nullable columns; the DDL is authoritative, this struct mirrors it.
type Item struct {
	ID               string  `json:"id"`
	ParentID         *string `json:"parent_id"`
	Ordinal          *int    `json:"ordinal"`
	Stage            string  `json:"stage"`
	Type             string  `json:"type"`
	Title            string  `json:"title"`
	Body             string  `json:"body"`
	Acceptance       string  `json:"acceptance"`
	Status           string  `json:"status"`
	Value            *int    `json:"value"`
	Confidence       *int    `json:"confidence"`
	Effort           *int    `json:"effort"`
	Score            int     `json:"score"`
	Why              string  `json:"why"`
	Repo             *string `json:"repo"`
	Paths            *string `json:"paths"`
	ClaimedBySession *string `json:"claimed_by_session"`
	ClaimedAt        *string `json:"claimed_at"`
	Assignee         *string `json:"assignee"`
	ExternalRef      *string `json:"external_ref"`
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
	ClosedAt         *string `json:"closed_at"`
}

// Dep mirrors the deps table one field per column. CreatedBy is the only
// nullable column.
type Dep struct {
	ItemID      string  `json:"item_id"`
	DependsOnID string  `json:"depends_on_id"`
	Type        string  `json:"type"`
	CreatedAt   string  `json:"created_at"`
	CreatedBy   *string `json:"created_by"`
}

// Event mirrors the events table one field per column. SessionID and Payload
// are the nullable columns; ID is the table's INTEGER PRIMARY KEY AUTOINCREMENT.
type Event struct {
	ID        int64   `json:"id"`
	ItemID    string  `json:"item_id"`
	Kind      string  `json:"kind"`
	Actor     string  `json:"actor"`
	SessionID *string `json:"session_id"`
	At        string  `json:"at"`
	Payload   *string `json:"payload"`
}

// ItemDetail is what `show` renders: an item plus its edges in both
// directions and its event history.
type ItemDetail struct {
	Item

	// DependsOn holds the deps rows where this item is item_id -- the edges
	// this item points FROM, i.e. what this item depends on.
	DependsOn []Dep `json:"depends_on"`

	// DependedOnBy holds the deps rows where this item is depends_on_id --
	// the edges this item points TO it, i.e. what depends on this item.
	DependedOnBy []Dep `json:"depended_on_by"`

	Events []Event `json:"events"`
}

// BlockedItem is a ready-eligible-shape item annotated with the id of the
// nearest item blocking it (READY-04) -- either the item itself, if it is
// directly blocked, or the nearest blocked ancestor in its parent-child tree.
type BlockedItem struct {
	Item
	BlockedBy string `json:"blocked_by"`
}

// TreeNode is one row of a parent-child subtree walk (store.Tree): the item,
// how deep beneath the requested root it sits, and whether it is blocked.
// It lives here, beside BlockedItem, for the same reason BlockedItem does --
// it is an Item plus one annotation, returned by a store method and rendered
// by a caller. Keeping it in internal/model is what lets internal/dash's
// render functions be pure (model in, HTML out) with no internal/store
// import at all, which is what makes DASH-06's golden test possible without
// a live database.
type TreeNode struct {
	Item
	Depth     int    `json:"depth"`
	Blocked   bool   `json:"blocked"`
	BlockedBy string `json:"blocked_by"`
}

// The four closed vocabularies below mirror the CHECK constraints in
// internal/store/schema.go's schemaDDL exactly. The DDL is authoritative;
// these constants exist so the CLI can validate a flag value without a SQL
// round-trip and without duplicating the list of legal values by hand.

// Item stages.
const (
	StageRaw    = "raw"
	StageTicket = "ticket"
)

// ValidStages is the lookup keep's CLI uses to validate a --stage value
// against the schema's CHECK(stage IN (...)) vocabulary without a query.
var ValidStages = map[string]bool{
	StageRaw:    true,
	StageTicket: true,
}

// StagesOrdered is the stage vocabulary in schema-enum order. See
// TypesOrdered's doc comment for why an ordered slice exists alongside the
// map: an ORDER is needed, not just a set, and this is the one place it is
// spelled out.
var StagesOrdered = []string{StageRaw, StageTicket}

// Item types.
const (
	TypeRoadmap = "roadmap"
	TypeEpic    = "epic"
	TypeTask    = "task"
	TypeBug     = "bug"
	TypeNote    = "note"
)

// ValidTypes is the lookup keep's CLI uses to validate a --type value
// against the schema's CHECK(type IN (...)) vocabulary without a query.
var ValidTypes = map[string]bool{
	TypeRoadmap: true,
	TypeEpic:    true,
	TypeTask:    true,
	TypeBug:     true,
	TypeNote:    true,
}

// ScoreMin and ScoreMax bound each of an item's three scoring inputs.
//
// The range was documented in internal/rank's comment and enforced only by
// cmd/keep's flag parser, which meant the store and therefore MCP accepted
// anything: effort=100 scored -89, below an unscored item's 0, breaking
// RANK-02's guarantee that a scored item always outranks an unscored one,
// and value=1000 pinned an item to the top of the queue for good. The
// numbers live here so the store's check and the MCP schema's advertised
// bounds cannot drift apart.
const (
	ScoreMin = 0
	ScoreMax = 10
)

// TypesOrdered is the type vocabulary in schema-enum order. It exists
// because two things need an ORDER, not just a set: backlog's grouping
// (internal/store/backlog.go's CASE expression) and the MCP tools' JSON
// Schema `enum` decoration (internal/mcp). Ranging over ValidTypes to build
// either would emit a different order on every process, which makes a demo
// transcript and a tools/list assertion flaky -- the same reason
// cmd/keep/add.go's validTypesList() has always spelled the order out by
// hand. This is now the ONE place it is spelled out.
var TypesOrdered = []string{TypeRoadmap, TypeEpic, TypeTask, TypeBug, TypeNote}

// Item statuses.
const (
	StatusOpen       = "open"
	StatusInProgress = "in_progress"
	StatusClosed     = "closed"
	StatusDropped    = "dropped"
)

// ValidStatuses is the lookup keep's CLI uses to validate a --status value
// against the schema's CHECK(status IN (...)) vocabulary without a query.
var ValidStatuses = map[string]bool{
	StatusOpen:       true,
	StatusInProgress: true,
	StatusClosed:     true,
	StatusDropped:    true,
}

// StatusesOrdered is the status vocabulary in schema-enum order. See
// TypesOrdered's doc comment for why.
var StatusesOrdered = []string{StatusOpen, StatusInProgress, StatusClosed, StatusDropped}

// Dep types.
const (
	DepBlocks      = "blocks"
	DepParentChild = "parent-child"
	DepRelated     = "related"
	DepDerivedFrom = "derived-from"
)

// ValidDepTypes is the lookup keep's CLI uses to validate a dep type value
// against the schema's CHECK(type IN (...)) vocabulary without a query.
var ValidDepTypes = map[string]bool{
	DepBlocks:      true,
	DepParentChild: true,
	DepRelated:     true,
	DepDerivedFrom: true,
}

// DepTypesOrdered is the dep type vocabulary in schema-enum order. See
// TypesOrdered's doc comment for why.
var DepTypesOrdered = []string{DepBlocks, DepParentChild, DepRelated, DepDerivedFrom}
