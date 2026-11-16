package store

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// baseSchemaVersion is the version schemaDDLv1 produces: the schema keep
// shipped before there was a ladder to climb. It never changes.
const baseSchemaVersion = 1

// migration is one rung. `to` is the version the store is at once apply
// returns; rungs run in slice order and each must produce exactly the
// previous version plus one, which validateMigrations asserts at init time
// rather than trusting the author to keep the slice sorted.
//
// apply runs inside the single transaction Migrate opens for the whole
// ladder. It must not commit, must not open its own connection, and must be
// safe to run against a store that real people have real data in, because
// every store it runs against already matters to someone.
type migration struct {
	to    int
	name  string
	apply func(ctx context.Context, tx *sql.Tx) error
}

// migrations is the ladder, in order. Appending a rung is the ONLY supported
// way to change the schema: editing schemaDDL without a matching rung
// produces a binary that creates one shape for new stores and refuses every
// old one, which is the failure this file exists to prevent.
//
// TestFreshInitMatchesMigratedV1 compares the schema a fresh Init produces
// against the schema schemaDDLv1-plus-this-ladder produces, object by object.
// That test keeps the two definitions in agreement, and it is why
// schemaDDLv1 is frozen rather than deleted.
var migrations = []migration{
	{
		to:   2,
		name: "full-text index over title, why, acceptance, paths and body",
		apply: func(ctx context.Context, tx *sql.Tx) error {
			// expandDepthSQL, not expandSchemaSQL: this rung runs while the
			// store is still at version 1, and expandSchemaSQL substitutes
			// the version this binary is at, which is not what an
			// in-progress migration should write. Migrate records the
			// version itself, one rung at a time. (It would also be an
			// initialization cycle, since schemaVersion is derived from this
			// slice.) TestFTSDDLCarriesNoVersionPlaceholder holds the
			// invariant that makes it safe.
			if _, err := tx.ExecContext(ctx, expandDepthSQL(ftsDDL)); err != nil {
				return fmt.Errorf("create the full-text index: %w", err)
			}
			// The triggers only see writes from here on. rebuild is what
			// indexes the items that were already there. Without it, search
			// would silently stop finding every item filed before the
			// upgrade, and because it still returns results, it would look
			// like it worked.
			if _, err := tx.ExecContext(ctx, `INSERT INTO items_fts(items_fts) VALUES('rebuild')`); err != nil {
				return fmt.Errorf("index the items that already exist: %w", err)
			}
			return nil
		},
	},
}

// schemaVersion is the version this binary creates and expects: the top of
// the ladder. It is derived rather than typed so that adding a rung cannot
// be half-done.
var schemaVersion = latestSchemaVersion()

func latestSchemaVersion() int {
	v := baseSchemaVersion
	for _, m := range migrations {
		v = m.to
	}
	return v
}

// validateMigrations is called from every entry point that consults the
// ladder. A gap or a repeat in `to` would silently skip or re-run a rung.
func validateMigrations() error {
	want := baseSchemaVersion + 1
	for _, m := range migrations {
		if m.to != want {
			return fmt.Errorf("migrations: rung %q declares to=%d, want %d -- the ladder must be contiguous from %d", m.name, m.to, want, baseSchemaVersion+1)
		}
		if m.name == "" {
			return fmt.Errorf("migrations: rung to=%d has no name; the name is what a failed migration reports", m.to)
		}
		want++
	}
	return nil
}

// storedSchemaVersion reads meta.schema_version without judging it. It
// returns ErrSchemaVersion for a store that was never initialized, since a
// read-write sql.Open fabricates an empty file rather than failing, and an
// empty file must not read as "version 0, migrate it".
func storedSchemaVersion(ctx context.Context, q rowQuerier) (int, error) {
	var got string
	if err := q.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'schema_version'").Scan(&got); err != nil {
		return 0, fmt.Errorf("read schema_version (path never initialized, or meta table missing): %w: %w", ErrSchemaVersion, err)
	}
	n, err := strconv.Atoi(got)
	if err != nil {
		return 0, fmt.Errorf("schema_version %q is not a number: %w: %w", got, ErrSchemaVersion, err)
	}
	return n, nil
}

// Pending reports which rungs a store at `from` still has to climb.
func pendingMigrations(from int) []migration {
	var out []migration
	for _, m := range migrations {
		if m.to > from {
			out = append(out, m)
		}
	}
	return out
}

// MigrationPlan describes what Migrate would do, so a caller can tell a
// person what is about to happen to their only copy of their data before it
// happens.
type MigrationPlan struct {
	From int
	To   int
	// Names are the rungs still to climb, in order.
	Names []string
	// Backup is where Migrate copied the store before touching it. Empty
	// when nothing had to be done, and on the plan PlanMigration returns:
	// planning takes no copy because it writes nothing.
	Backup string
}

// PlanMigration inspects the store at path and reports the work needed to
// bring it to this binary's schema version. It opens read-only and never
// writes, so it is safe to call on a store another process holds.
//
// A store already at the current version returns a plan with From == To and
// no names, not an error. "Nothing to do" is an answer, not a failure.
func PlanMigration(ctx context.Context, path string) (MigrationPlan, error) {
	if err := validateMigrations(); err != nil {
		return MigrationPlan{}, err
	}
	s, err := open(path, OpenOptions{ReadOnly: true})
	if err != nil {
		return MigrationPlan{}, err
	}
	defer s.db.Close()

	from, err := storedSchemaVersion(ctx, s.db)
	if err != nil {
		return MigrationPlan{}, err
	}
	if from > schemaVersion {
		return MigrationPlan{}, fmt.Errorf(
			"store is at schema_version %d, newer than this binary's %d -- upgrade keep rather than downgrading the store: %w",
			from, schemaVersion, ErrSchemaVersion)
	}

	plan := MigrationPlan{From: from, To: schemaVersion}
	for _, m := range pendingMigrations(from) {
		plan.Names = append(plan.Names, m.name)
	}
	return plan, nil
}

// Migrate brings the store at path up to this binary's schema version and
// returns the plan it carried out.
//
// It takes a copy of the database file first, and says where it put it. The
// store is frequently the only record of why a piece of work exists (a
// captured plan lives in items.body and nowhere else), so a migration
// that goes wrong must be recoverable by copying one file back, not by
// reasoning about half-applied DDL. The copy is taken after a TRUNCATE
// checkpoint so the -wal sidecar's contents are folded into the main file
// and the backup is complete on its own.
//
// The whole ladder runs in one transaction. Climbing three rungs and failing
// on the fourth must leave the store at its original version, not at an
// intermediate one no binary in the world declares support for.
func Migrate(ctx context.Context, path string) (MigrationPlan, error) {
	plan, err := PlanMigration(ctx, path)
	if err != nil {
		return MigrationPlan{}, err
	}
	if len(plan.Names) == 0 {
		return plan, nil
	}

	s, err := open(path, OpenOptions{})
	if err != nil {
		return MigrationPlan{}, err
	}
	defer s.db.Close()

	backup, err := backupStore(ctx, s, path, plan.From)
	if err != nil {
		return MigrationPlan{}, err
	}
	plan.Backup = backup

	err = s.withTx(ctx, func(tx *sql.Tx) error {
		for _, m := range pendingMigrations(plan.From) {
			if err := m.apply(ctx, tx); err != nil {
				return fmt.Errorf("migration %q (to schema_version %d) failed, store left at %d, backup at %s: %w",
					m.name, m.to, plan.From, backup, err)
			}
			if _, err := tx.ExecContext(ctx,
				"UPDATE meta SET value = ? WHERE key = 'schema_version'", strconv.Itoa(m.to)); err != nil {
				return fmt.Errorf("migration %q: record schema_version %d: %w", m.name, m.to, err)
			}
		}
		return nil
	})
	if err != nil {
		return MigrationPlan{}, err
	}
	return plan, nil
}

// backupStore checkpoints the WAL into the main database file and copies it
// to a sibling path that does not already exist. It never overwrites: a
// previous failed attempt's backup is more valuable than a tidy directory.
func backupStore(ctx context.Context, s *Store, path string, from int) (string, error) {
	if _, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return "", fmt.Errorf("checkpoint before backup: %w", err)
	}

	base := fmt.Sprintf("%s.v%d.bak", path, from)
	dest := base
	for n := 1; ; n++ {
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			break
		} else if err != nil {
			return "", fmt.Errorf("check backup path %s: %w", dest, err)
		}
		if n > 100 {
			return "", fmt.Errorf("backup: %s and 100 numbered siblings all exist; move them aside", base)
		}
		dest = fmt.Sprintf("%s.%d", base, n)
	}

	if err := copyFile(path, dest); err != nil {
		return "", fmt.Errorf("back up store to %s: %w", dest, err)
	}
	return dest, nil
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dest)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dest)
		return err
	}
	return out.Close()
}
