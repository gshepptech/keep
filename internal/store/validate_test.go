package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gshepptech/keep/internal/model"
)

// TestEveryTerminalVerbRequiresAReason proves the store, not a surface,
// refuses a blank reason. If `--reason` were enforced only in cmd/keep, the
// MCP `close` tool would record a close with no why at all every time an
// agent omitted the argument.
//
// All four verbs are checked in one table rather than four tests, because
// the rule is one decision applied to all four, not four separate ones.
func TestEveryTerminalVerbRequiresAReason(t *testing.T) {
	ctx := context.Background()

	verbs := []struct {
		name string
		// prepare puts an item into the status the verb needs.
		prepare func(t *testing.T, s *Store, id string)
		call    func(s *Store, id, reason string) error
	}{
		{
			name:    "close",
			prepare: func(t *testing.T, s *Store, id string) {},
			call: func(s *Store, id, reason string) error {
				_, err := s.CloseItem(ctx, CloseItemInput{ID: id, Reason: reason, Actor: "t"})
				return err
			},
		},
		{
			name:    "drop",
			prepare: func(t *testing.T, s *Store, id string) {},
			call: func(s *Store, id, reason string) error {
				_, err := s.DropItem(ctx, DropItemInput{ID: id, Reason: reason, Actor: "t"})
				return err
			},
		},
		{
			name: "unclaim",
			prepare: func(t *testing.T, s *Store, id string) {
				if _, err := s.StartItem(ctx, StartItemInput{ID: id, Actor: "t", SessionID: "sess"}); err != nil {
					t.Fatalf("StartItem: %v", err)
				}
			},
			call: func(s *Store, id, reason string) error {
				_, err := s.UnclaimItem(ctx, UnclaimItemInput{ID: id, Reason: reason, Actor: "t"})
				return err
			},
		},
		{
			name: "reopen",
			prepare: func(t *testing.T, s *Store, id string) {
				if _, err := s.CloseItem(ctx, CloseItemInput{ID: id, Reason: "done", Actor: "t"}); err != nil {
					t.Fatalf("CloseItem: %v", err)
				}
			},
			call: func(s *Store, id, reason string) error {
				_, err := s.ReopenItem(ctx, ReopenItemInput{ID: id, Reason: reason, Actor: "t"})
				return err
			},
		},
	}

	// Whitespace is the same absence as emptiness. A bare `!= ""` check
	// would let " " through and record a reason nobody can read.
	blanks := map[string]string{"empty": "", "whitespace": "   \t\n "}

	for _, v := range verbs {
		for blankName, blank := range blanks {
			t.Run(v.name+"/"+blankName, func(t *testing.T) {
				s := openTestStore(t)
				item := mustAdd(t, s, AddItemInput{Title: "subject", Actor: "t"})
				v.prepare(t, s, item.ID)

				before := countEvents(t, s)
				statusBefore := statusOf(t, s, item.ID)

				err := v.call(s, item.ID, blank)
				if !errors.Is(err, ErrEmptyReason) {
					t.Fatalf("%s with a %s reason = %v, want ErrEmptyReason", v.name, blankName, err)
				}

				// Refused means nothing happened, not "happened and then
				// complained".
				if after := countEvents(t, s); after != before {
					t.Errorf("a refused %s wrote %d event(s)", v.name, after-before)
				}
				if after := statusOf(t, s, item.ID); after != statusBefore {
					t.Errorf("a refused %s moved the status from %q to %q", v.name, statusBefore, after)
				}
			})
		}

		// And the same call with a reason succeeds, so the test above is
		// refusing the blank rather than the verb.
		t.Run(v.name+"/with a reason", func(t *testing.T) {
			s := openTestStore(t)
			item := mustAdd(t, s, AddItemInput{Title: "subject", Actor: "t"})
			v.prepare(t, s, item.ID)
			if err := v.call(s, item.ID, "a real reason"); err != nil {
				t.Fatalf("%s with a reason = %v, want nil", v.name, err)
			}
		})
	}
}

// TestClosedVocabulariesAreRefusedByName checks that an invalid type or stage
// is refused with an error naming the value and the allowed set. The schema's
// CHECK constraints would refuse these too, but cannot say which value was
// wrong in words a caller can act on.
//
// Without the Go-side check, the MCP `add` tool would answer an invalid
// type with "constraint failed: CHECK constraint failed: type IN (...)
// (275)", SQLite's text forwarded as a generic protocol error, which a
// model cannot tell apart from a broken store.
func TestClosedVocabulariesAreRefusedByName(t *testing.T) {
	ctx := context.Background()

	t.Run("add", func(t *testing.T) {
		s := openTestStore(t)
		before := countItems(t, s)

		_, err := s.AddItem(ctx, AddItemInput{Title: "x", Actor: "t", Type: "nonsense"})
		if !errors.Is(err, ErrInvalidType) {
			t.Fatalf("AddItem(type=nonsense) = %v, want ErrInvalidType", err)
		}
		assertNamesTheValueAndTheLegalOnes(t, err, "nonsense", model.TypesOrdered)

		_, err = s.AddItem(ctx, AddItemInput{Title: "x", Actor: "t", Stage: "nonsense"})
		if !errors.Is(err, ErrInvalidStage) {
			t.Fatalf("AddItem(stage=nonsense) = %v, want ErrInvalidStage", err)
		}
		assertNamesTheValueAndTheLegalOnes(t, err, "nonsense", model.StagesOrdered)

		if after := countItems(t, s); after != before {
			t.Errorf("a refused AddItem wrote %d item(s)", after-before)
		}
	})

	t.Run("update", func(t *testing.T) {
		s := openTestStore(t)
		item := mustAdd(t, s, AddItemInput{Title: "x", Actor: "t"})

		bad := "nonsense"
		if _, err := s.UpdateItem(ctx, UpdateItemInput{ID: item.ID, Actor: "t", Type: &bad}); !errors.Is(err, ErrInvalidType) {
			t.Errorf("UpdateItem(type=nonsense) = %v, want ErrInvalidType", err)
		}
		if _, err := s.UpdateItem(ctx, UpdateItemInput{ID: item.ID, Actor: "t", Stage: &bad}); !errors.Is(err, ErrInvalidStage) {
			t.Errorf("UpdateItem(stage=nonsense) = %v, want ErrInvalidStage", err)
		}
	})

	// An empty value is not invalid. It means "the caller did not set this"
	// and the insert applies its own default. Refusing it here would
	// break every call that omits the field, which is most of them.
	t.Run("unset is not invalid", func(t *testing.T) {
		s := openTestStore(t)
		if _, err := s.AddItem(ctx, AddItemInput{Title: "x", Actor: "t"}); err != nil {
			t.Fatalf("AddItem with no type or stage = %v, want nil", err)
		}
	})

	// Every legal value is accepted, so the check is refusing what is
	// outside the vocabulary rather than refusing everything.
	t.Run("every legal value is accepted", func(t *testing.T) {
		s := openTestStore(t)
		for _, ty := range model.TypesOrdered {
			if _, err := s.AddItem(ctx, AddItemInput{Title: "x " + ty, Actor: "t", Type: ty}); err != nil {
				t.Errorf("AddItem(type=%s) = %v, want nil", ty, err)
			}
		}
		for _, st := range model.StagesOrdered {
			if _, err := s.AddItem(ctx, AddItemInput{Title: "y " + st, Actor: "t", Stage: st}); err != nil {
				t.Errorf("AddItem(stage=%s) = %v, want nil", st, err)
			}
		}
	})
}

// assertNamesTheValueAndTheLegalOnes checks that err names the offending
// value and every legal one.
func assertNamesTheValueAndTheLegalOnes(t *testing.T, err error, value string, legal []string) {
	t.Helper()
	if !strings.Contains(err.Error(), value) {
		t.Errorf("the error does not name the offending value %q: %v", value, err)
	}
	for _, l := range legal {
		if !strings.Contains(err.Error(), l) {
			t.Errorf("the error does not name the legal value %q, so a caller cannot correct it: %v", l, err)
		}
	}
}

// statusOf reads one item's status, for the "a refused call changed
// nothing" assertions above.
func statusOf(t *testing.T, s *Store, id string) string {
	t.Helper()
	detail, err := s.GetItem(context.Background(), id)
	if err != nil {
		t.Fatalf("GetItem(%s): %v", id, err)
	}
	return detail.Status
}
