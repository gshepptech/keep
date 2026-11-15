package rank

// scoreShift keeps every *scored* item's score >= 1, so an unscored item's
// score of 0 always sorts after it.
const scoreShift = 11

// Score computes an item's ranking key from optional value/confidence/effort
// inputs (each 0-10 when present). All three nil means "unscored": Score
// returns 0. Any non-nil input makes the item "scored"; a nil input among a
// scored triple counts as 0 in the arithmetic. Only the all-nil case gets
// score 0.
func Score(value, confidence, effort *int) int {
	if value == nil && confidence == nil && effort == nil {
		return 0
	}
	return deref(value) + deref(confidence) - deref(effort) + scoreShift
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
