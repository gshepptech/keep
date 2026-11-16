package store

import "os"

// Env reads one of keep's own environment variables by its unprefixed name:
// Env("DB") reads $KEEP_DB.
//
// It falls back to the older ROUNCE_-prefixed name for the same variable, so
// a shell profile that exports ROUNCE_ACTOR keeps attributing work to the
// right actor instead of to $USER. The older name is read only when the new
// one is absent, so a machine that sets KEEP_ variables always gets them.
//
// Empty and unset are the same thing here, which is the convention the rest
// of this package uses: resolveActor treats KEEP_ACTOR="" as unset, and every
// nullable column treats "" as absent. So KEEP_ACTOR="" falls through to
// ROUNCE_ACTOR when that is set.
func Env(name string) string {
	if v := os.Getenv("KEEP_" + name); v != "" {
		return v
	}
	return os.Getenv("ROUNCE_" + name)
}
