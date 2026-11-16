package store

import "os"

// Env reads one of keep's own environment variables by its unprefixed name:
// Env("DB") reads $KEEP_DB.
//
// When KEEP_<name> is empty it reads ROUNCE_<name>. Empty and unset are the
// same thing here, which is the convention the rest of this package uses:
// resolveActor treats KEEP_ACTOR="" as unset, and every nullable column
// treats "" as absent.
func Env(name string) string {
	if v := os.Getenv("KEEP_" + name); v != "" {
		return v
	}
	return os.Getenv("ROUNCE_" + name)
}
