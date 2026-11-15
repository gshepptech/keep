package store

import (
	"context"
	"errors"
	"testing"
)

// TestAnItemCannotBeLeftWithNothingToCallItBy checks that both add and update
// refuse a blank title.
//
// An item with no title is not a partially-filled record; it is a hole in
// the queue. `ready`, `list`, `search`, the board's rows and every MCP tool
// show the title and nothing else identifying, so the only way to refer to
// such a row is by an id the design deliberately made opaque.
//
// This is guarded in the store rather than in `add`, because five places
// build an AddItemInput -- add, capture, two MCP tools and the board -- and
// a rule enforced in one of them is a rule that holds until somebody uses a
// different surface. It was found through the CLI (`keep add --help` filed
// an item called "--help"), but the CLI was only the door it came through.
//
// Whitespace is empty. "   " renders as an unlabelled row exactly like ""
// does, and a validator that can tell them apart is drawing a line no
// reader can see.
func TestAnItemCannotBeLeftWithNothingToCallItBy(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	empty := []struct{ name, title string }{
		{"empty string", ""},
		{"spaces", "   "},
		{"a tab", "\t"},
		{"newlines", "\n\n"},
	}

	t.Run("add refuses", func(t *testing.T) {
		for _, c := range empty {
			t.Run(c.name, func(t *testing.T) {
				_, err := s.AddItem(ctx, AddItemInput{Actor: "tester", Title: c.title})
				if !errors.Is(err, ErrEmptyTitle) {
					t.Fatalf("AddItem(title=%q) = %v, want ErrEmptyTitle", c.title, err)
				}
			})
		}
	})

	// The gap that made this worth its own subtest: closing add alone would
	// have left the same hole reachable in two commands instead of one.
	t.Run("update refuses to blank an existing title", func(t *testing.T) {
		item, err := s.AddItem(ctx, AddItemInput{Actor: "tester", Title: "a real title"})
		if err != nil {
			t.Fatalf("AddItem: %v", err)
		}
		for _, c := range empty {
			t.Run(c.name, func(t *testing.T) {
				blank := c.title
				_, err := s.UpdateItem(ctx, UpdateItemInput{
					ID: item.ID, Actor: "tester", Title: &blank,
				})
				if !errors.Is(err, ErrEmptyTitle) {
					t.Fatalf("UpdateItem(title=%q) = %v, want ErrEmptyTitle", c.title, err)
				}
			})
		}
		// And the row is untouched, not half-written by a transaction that
		// got partway before the guard.
		got, err := s.GetItem(ctx, item.ID)
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		if got.Title != "a real title" {
			t.Fatalf("title is now %q; a refused update rewrote the row", got.Title)
		}
	})

	// A guard that refuses everything is not a guard. Titles that merely
	// contain whitespace, or look like a flag, are ordinary and must pass:
	// `keep add -- --weird-title` is a documented invocation.
	t.Run("real titles still pass", func(t *testing.T) {
		for _, title := range []string{"a", " leading space", "trailing ", "--weird-title", "0"} {
			if _, err := s.AddItem(ctx, AddItemInput{Actor: "tester", Title: title}); err != nil {
				t.Errorf("AddItem(title=%q) = %v, want it accepted", title, err)
			}
		}
	})
}
