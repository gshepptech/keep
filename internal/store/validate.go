package store

import (
	"fmt"
	"strings"

	"github.com/gshepptech/keep/internal/model"
)

// The closed-vocabulary and required-field checks every entry point shares.
//
// They live here, in the store, rather than in each surface -- which is
// where they used to live and where they drifted. README principle 2 says
// the CLI, the MCP server and the dashboard are thin wrappers with no
// behaviour of their own; a validation rule enforced in one wrapper is
// behaviour, and the proof is what these three surfaces were actually doing
// with `--reason` before this file existed:
//
//	CLI         refused an empty reason, exit 2
//	MCP         accepted it and recorded a close with no why at all
//	dashboard   refused it for drop and unclaim, supplied its own for close
//
// cmd/keep keeps its own pre-checks. That is not a second copy of the rule
// for its own sake: the CLI's checks run BEFORE a store is opened, they
// classify as usage errors (exit 2, not 1), and they print the list of legal
// values, none of which the store can do from where it sits. The store is
// what makes the rule true; the CLI's version is what makes the message
// good.

// requireReason refuses a blank reason for the four verbs that record a
// terminal or near-terminal fact about a piece of work.
//
// verb names the caller so the message reads as the command a person ran.
// The reason is trimmed first: a reason of three spaces is the same absence
// as an empty one, and it would otherwise satisfy a bare != "" check while
// recording nothing.
func requireReason(verb, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%s: %w -- say why, on the record this writes", verb, ErrEmptyReason)
	}
	return nil
}

// checkType refuses a type outside the closed vocabulary. An empty value is
// allowed and means "the caller did not set it"; the insert applies its own
// default.
func checkType(t string) error {
	if t == "" || model.ValidTypes[t] {
		return nil
	}
	return fmt.Errorf("type %q is not one of %s: %w", t, strings.Join(model.TypesOrdered, ", "), ErrInvalidType)
}

// checkStage refuses a stage outside the closed vocabulary, on the same
// terms as checkType.
func checkStage(st string) error {
	if st == "" || model.ValidStages[st] {
		return nil
	}
	return fmt.Errorf("stage %q is not one of %s: %w", st, strings.Join(model.StagesOrdered, ", "), ErrInvalidStage)
}
