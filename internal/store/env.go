package store

import "os"

// Env reads one of keep's own environment variables by its unprefixed name:
// Env("DB") reads $KEEP_DB.
//
// It falls back to the ROUNCE_-prefixed name the same variable had before the
// project was renamed. The rename is a change to what this tool is called,
// not to what it does, and a person whose shell profile already exports
// ROUNCE_ACTOR should not have their audit trail quietly start attributing
// work to $USER instead. The old name is read only when the new one is
// absent, so a machine that has moved on is never dragged back.
//
// Empty and unset are the same thing here, which is the convention the rest
// of this package uses: resolveActor treats KEEP_ACTOR="" as unset, and every
// nullable column treats "" as absent. Without that, exporting KEEP_ACTOR=""
// to clear it would fall through to ROUNCE_ACTOR and appear not to work.
func Env(name string) string {
	if v := os.Getenv("KEEP_" + name); v != "" {
		return v
	}
	return os.Getenv("ROUNCE_" + name)
}
