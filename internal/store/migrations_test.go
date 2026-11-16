package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// schemaObjects reads back every object SQLite actually created (tables,
// indexes, views, triggers) with the exact SQL text it stored for each.
// Comparing this between two stores is a shape comparison, not a name
// comparison, and that distinction matters: CREATE TABLE IF NOT EXISTS
// matches on name and would happily leave two stores with the same table
// names and different columns.
//
// Autoindexes are excluded: SQLite names them sqlite_autoindex_<table>_<n>
// and stores NULL sql for them, so they carry no information this comparison
// does not already get from the CREATE TABLE text that implies them.
func schemaObjects(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT type, name, COALESCE(sql, '') FROM sqlite_master
		 WHERE name NOT LIKE 'sqlite_autoindex_%'
		 ORDER BY type, name`)
	if err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var typ, name, ddl string
		if err := rows.Scan(&typ, &name, &ddl); err != nil {
			t.Fatalf("scan sqlite_master: %v", err)
		}
		out = append(out, fmt.Sprintf("%s %s\n%s", typ, name, strings.TrimSpace(ddl)))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("sqlite_master rows: %v", err)
	}
	return out
}

// TestFreshInitMatchesMigratedV1 checks that a brand-new Init and a v1
// store climbed all the way up the migration ladder end with identical
// schemas, compared object by object.
//
// Without it, `migrations` and `schemaDDL` are two independent descriptions
// of one shape, and the only thing keeping them in agreement is whoever
// remembers to edit both. The failure that causes is quiet and late: new
// installs work, upgraded installs work until some query touches the column
// the ladder forgot, and by then the store has months of data in it.
func TestFreshInitMatchesMigratedV1(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	fresh := filepath.Join(dir, "fresh.sqlite")
	fs, err := Init(fresh)
	if err != nil {
		t.Fatalf("Init(fresh): %v", err)
	}
	t.Cleanup(func() { fs.Close() })

	old := filepath.Join(dir, "old.sqlite")
	os_, err := initWithDDL(old, schemaDDLv1, baseSchemaVersion)
	if err != nil {
		t.Fatalf("initWithDDL(v1): %v", err)
	}
	os_.Close()

	plan, err := Migrate(ctx, old)
	if err != nil {
		t.Fatalf("Migrate(v1 store): %v", err)
	}
	if plan.From != baseSchemaVersion {
		t.Fatalf("plan.From = %d, want %d", plan.From, baseSchemaVersion)
	}
	if plan.To != schemaVersion {
		t.Fatalf("plan.To = %d, want %d", plan.To, schemaVersion)
	}

	ms, err := Open(old, OpenOptions{})
	if err != nil {
		t.Fatalf("Open(migrated): %v", err)
	}
	t.Cleanup(func() { ms.Close() })

	want, got := schemaObjects(t, fs), schemaObjects(t, ms)
	if len(want) != len(got) {
		t.Fatalf("fresh store has %d schema objects, migrated v1 store has %d:\nfresh:\n%s\nmigrated:\n%s",
			len(want), len(got), strings.Join(want, "\n---\n"), strings.Join(got, "\n---\n"))
	}
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("schema object %d differs.\nfresh:\n%s\n\nmigrated:\n%s", i, want[i], got[i])
		}
	}
}

// TestMigrationLadderIsContiguous checks the slice itself rather than its
// effect. A rung whose `to` repeats or skips would run twice or not at all,
// and both failures show up as a schema that is subtly wrong rather than as
// an error at the point of the mistake.
func TestMigrationLadderIsContiguous(t *testing.T) {
	if err := validateMigrations(); err != nil {
		t.Fatalf("validateMigrations: %v", err)
	}
	if got := latestSchemaVersion(); got != schemaVersion {
		t.Fatalf("latestSchemaVersion() = %d, schemaVersion = %d -- they must not drift", got, schemaVersion)
	}
	if len(migrations) > 0 && migrations[len(migrations)-1].to != schemaVersion {
		t.Fatalf("top rung is to=%d but schemaVersion is %d", migrations[len(migrations)-1].to, schemaVersion)
	}
}

// TestOpenOlderStoreAsksForMigration proves an older store is told to
// migrate, not refused with the same flat error as a corrupted one. A flat
// refusal would brick every existing store on the first schema change.
func TestOpenOlderStoreAsksForMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")
	s, err := Init(path)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := s.db.ExecContext(context.Background(),
		"UPDATE meta SET value = '0' WHERE key = 'schema_version'"); err != nil {
		t.Fatalf("age the store: %v", err)
	}
	s.Close()

	_, err = Open(path, OpenOptions{})
	if !errors.Is(err, ErrNeedsMigration) {
		t.Fatalf("Open(older store) = %v, want ErrNeedsMigration", err)
	}
	if errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("Open(older store) also matched ErrSchemaVersion; the two outcomes must be distinguishable")
	}
	if !strings.Contains(err.Error(), "keep migrate") {
		t.Errorf("error does not name the command that fixes it: %v", err)
	}
}

// TestOpenNewerStoreRefusesWithoutOfferingMigration checks that a store
// from a newer keep is refused without suggesting `keep migrate`. A store
// written by a newer keep cannot be repaired by an older one, and
// offering `keep migrate` there would send someone round a loop that cannot
// terminate.
func TestOpenNewerStoreRefusesWithoutOfferingMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.sqlite")
	s, err := Init(path)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := s.db.ExecContext(context.Background(),
		"UPDATE meta SET value = ? WHERE key = 'schema_version'", schemaVersion+1); err != nil {
		t.Fatalf("advance the store: %v", err)
	}
	s.Close()

	_, err = Open(path, OpenOptions{})
	if !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("Open(newer store) = %v, want ErrSchemaVersion", err)
	}
	if errors.Is(err, ErrNeedsMigration) {
		t.Fatalf("Open(newer store) offered migration; nothing this binary can run would fix it")
	}
	if _, err := Migrate(context.Background(), path); !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("Migrate(newer store) = %v, want ErrSchemaVersion", err)
	}
}

// TestMigrateAtCurrentVersionIsANoOp checks that migrating an up-to-date
// store succeeds and changes nothing. A migrate that errored on an
// up-to-date store would make it unusable in any
// script that runs it unconditionally, and would take a pointless backup
// copy of the store every time.
func TestMigrateAtCurrentVersionIsANoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.sqlite")
	s, err := Init(path)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	s.Close()

	plan, err := Migrate(context.Background(), path)
	if err != nil {
		t.Fatalf("Migrate(current) = %v, want nil", err)
	}
	if len(plan.Names) != 0 {
		t.Fatalf("plan.Names = %v, want none", plan.Names)
	}
	if plan.Backup != "" {
		t.Fatalf("plan.Backup = %q; a no-op migration must not copy the store", plan.Backup)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".bak") {
			t.Errorf("no-op migration left a backup file %s", e.Name())
		}
	}
}

// withTestMigrations swaps in a temporary ladder for the rest of the test.
// TestMigrateBacksUpBeforeTouchingTheStore and
// TestFailingMigrationLeavesTheStoreAtItsOriginalVersion both need a rung to
// exercise while the real ladder may be empty, so the machinery is tested
// on its own terms rather than only through whichever migration exists.
func withTestMigrations(t *testing.T, m []migration) {
	t.Helper()
	oldMigrations, oldVersion := migrations, schemaVersion
	migrations = m
	schemaVersion = latestSchemaVersion()
	t.Cleanup(func() {
		migrations, schemaVersion = oldMigrations, oldVersion
	})
}

func TestMigrateBacksUpBeforeTouchingTheStore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "db.sqlite")

	// A genuine version-1 store, because withTestMigrations replaces the
	// whole ladder and a ladder must start at baseSchemaVersion+1. Building
	// this on a fresh Init instead would need rungs numbered above the real
	// ladder's top, which validateMigrations correctly refuses.
	s, err := initWithDDL(path, schemaDDLv1, baseSchemaVersion)
	if err != nil {
		t.Fatalf("initWithDDL(v1): %v", err)
	}
	if _, err := s.AddItem(ctx, AddItemInput{Title: "worth not losing", Actor: "tester"}); err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	before := baseSchemaVersion
	s.Close()

	withTestMigrations(t, []migration{{
		to:   before + 1,
		name: "add a canary table",
		apply: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "CREATE TABLE canary (n INTEGER)")
			return err
		},
	}})

	plan, err := Migrate(ctx, path)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if plan.Backup == "" {
		t.Fatal("Migrate reported no backup path; a migration that cannot be undone by copying one file back is not recoverable")
	}
	if _, err := os.Stat(plan.Backup); err != nil {
		t.Fatalf("backup %s: %v", plan.Backup, err)
	}

	// The backup must be a working store at the ORIGINAL version, not a
	// zero-length file or a copy taken after the change. An empty ladder
	// puts schemaVersion back to baseSchemaVersion, which is what the
	// backup is at.
	withTestMigrations(t, nil)
	backup, err := Open(plan.Backup, OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("Open(backup) at the original version: %v", err)
	}
	defer backup.Close()

	items, err := backup.ListItems(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("ListItems(backup): %v", err)
	}
	if len(items) != 1 || items[0].Title != "worth not losing" {
		t.Fatalf("backup holds %d items %v, want the one that was there before the migration", len(items), items)
	}
	var canary int
	err = backup.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'canary'").Scan(&canary)
	if err != nil {
		t.Fatalf("look for canary in backup: %v", err)
	}
	if canary != 0 {
		t.Error("backup already contains the migration's own table; the copy was taken too late to be a backup")
	}
}

func TestFailingMigrationLeavesTheStoreAtItsOriginalVersion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db.sqlite")
	s, err := initWithDDL(path, schemaDDLv1, baseSchemaVersion)
	if err != nil {
		t.Fatalf("initWithDDL(v1): %v", err)
	}
	before := baseSchemaVersion
	s.Close()

	withTestMigrations(t, []migration{
		{
			to:   before + 1,
			name: "first rung, succeeds",
			apply: func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, "CREATE TABLE rung_one (n INTEGER)")
				return err
			},
		},
		{
			to:   before + 2,
			name: "second rung, fails",
			apply: func(ctx context.Context, tx *sql.Tx) error {
				return errors.New("deliberate failure")
			},
		},
	})

	if _, err := Migrate(ctx, path); err == nil {
		t.Fatal("Migrate = nil, want the second rung's failure")
	} else if !strings.Contains(err.Error(), "second rung, fails") {
		t.Errorf("error does not name the rung that failed: %v", err)
	}

	// Back to a world with no ladder: the store must still open at the
	// version it started at, with the first rung's table rolled back.
	withTestMigrations(t, nil)
	reopened, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatalf("Open after a failed migration: %v -- the store must be left exactly as it was", err)
	}
	defer reopened.Close()

	var rungOne int
	if err := reopened.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'rung_one'").Scan(&rungOne); err != nil {
		t.Fatalf("look for rung_one: %v", err)
	}
	if rungOne != 0 {
		t.Error("the first rung's table survived a failed ladder; the whole ladder must run in one transaction")
	}
}

// TestBackupNeverOverwritesAnEarlierOne checks that a second migration
// attempt writes a new backup rather than replacing the first one, which
// after a failed attempt is the most valuable file on the disk.
func TestBackupNeverOverwritesAnEarlierOne(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "db.sqlite")
	s, err := initWithDDL(path, schemaDDLv1, baseSchemaVersion)
	if err != nil {
		t.Fatalf("initWithDDL(v1): %v", err)
	}
	before := baseSchemaVersion
	s.Close()

	rung := []migration{{
		to:    before + 1,
		name:  "no-op rung",
		apply: func(ctx context.Context, tx *sql.Tx) error { return nil },
	}}

	withTestMigrations(t, rung)
	first, err := Migrate(ctx, path)
	if err != nil {
		t.Fatalf("first Migrate: %v", err)
	}

	// Put the store back to where it was so a second migration is due.
	s2, err := open(path, OpenOptions{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := s2.db.ExecContext(ctx, "UPDATE meta SET value = ? WHERE key = 'schema_version'", before); err != nil {
		t.Fatalf("rewind version: %v", err)
	}
	s2.db.Close()

	second, err := Migrate(ctx, path)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if second.Backup == first.Backup {
		t.Fatalf("second migration reused backup path %s, overwriting the first attempt's copy", first.Backup)
	}
	for _, p := range []string{first.Backup, second.Backup} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("backup %s missing: %v", p, err)
		}
	}
}

// TestFTSDDLCarriesNoVersionPlaceholder checks that ftsDDL contains no
// schema-version placeholder, the invariant that lets
// migrations rung 2 expand ftsDDL with expandDepthSQL rather than
// expandSchemaSQL. Rung 2 runs while the store is still at the old version,
// so substituting this binary's version into it would write the wrong
// number; and expandSchemaSQL cannot be called from the migrations slice
// anyway, because schemaVersion is derived from that slice. The rung is
// correct only for as long as ftsDDL needs no version substitution, and
// that is not obvious from either file.
func TestFTSDDLCarriesNoVersionPlaceholder(t *testing.T) {
	if strings.Contains(ftsDDL, "$SCHEMAVERSION$") {
		t.Fatal("ftsDDL now contains $SCHEMAVERSION$, but migrations rung 2 expands it with expandDepthSQL, which does not substitute it -- the placeholder would reach SQLite verbatim")
	}
}

// TestSchemaDDLExpandsEveryPlaceholder checks that no placeholder survives
// expansion of the DDL. An unsubstituted placeholder is not a compile error
// and fails no other test; it is a SQLite syntax error at Init time, or a
// literal string written into meta.schema_version that every later Open
// compares against.
func TestSchemaDDLExpandsEveryPlaceholder(t *testing.T) {
	for _, ph := range []string{"$MAXDEPTH$", "$SCHEMAVERSION$"} {
		if strings.Contains(schemaDDL, ph) {
			t.Errorf("schemaDDL still contains %s after expansion", ph)
		}
		if strings.Contains(schemaDDLv1, ph) {
			t.Errorf("schemaDDLv1 still contains %s after expansion", ph)
		}
	}
	if !strings.Contains(schemaDDL, "'"+strconv.Itoa(schemaVersion)+"'") {
		t.Errorf("schemaDDL does not write schema_version %d; a fresh store would open at the wrong version", schemaVersion)
	}
}
