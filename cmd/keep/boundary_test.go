package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// forbiddenImports are import paths that would give a package above
// internal/store direct access to a SQL driver, which the store boundary
// forbids: nothing above internal/store ever sees a *sql.DB, a row, or a
// query string.
var forbiddenImports = []string{
	"database/sql",
	"modernc.org/sqlite",
	"modernc.org/sqlite/lib",
}

// forbiddenSQLLiterals are SQL keyword fragments that must never appear as a
// string literal outside internal/store. A file could avoid importing a SQL
// driver directly and still leak SQL upward by building a query string to
// hand off elsewhere; this check catches that even without an import to flag.
var forbiddenSQLLiterals = []string{
	"SELECT ",
	"INSERT INTO",
	"CREATE TABLE",
	"UPDATE ",
	"DELETE FROM",
	"PRAGMA ",
}

// storeDirPrefix is internal/store's path relative to the module root, with
// a trailing separator so a prefix match cannot also match, say,
// internal/store2.
var storeDirPrefix = "internal" + string(filepath.Separator) + "store" + string(filepath.Separator)

// TestNoSQLAboveStore checks that internal/store is the only package
// containing SQL, mechanically rather than by review. It parses every
// non-vendored .go file in the module and
// fails if any file outside internal/store imports a SQL driver package or
// contains a SQL keyword string literal.
func TestNoSQLAboveStore(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed -- cannot locate this test's own file to derive the module root")
	}

	// This file lives at <module root>/cmd/keep/boundary_test.go, so two
	// levels up is the module root. That holds whatever working directory
	// the package runs from, and the zero-file guard below fails loudly if
	// the module layout changes, rather than silently scanning nothing.
	moduleRoot, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}

	fset := token.NewFileSet()
	filesWalked := 0
	coveredPkgs := map[string]int{}

	walkErr := filepath.WalkDir(moduleRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// A broken walk root (e.g. moduleRoot itself does not exist)
			// surfaces here with a nil DirEntry. It is not propagated as a
			// hard failure: the walk finishes having found nothing, and the
			// filesWalked == 0 guard below turns "found nothing" into a
			// failure, rather than an incidental I/O error doing it.
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		filesWalked++

		rel, relErr := filepath.Rel(moduleRoot, path)
		if relErr != nil {
			return relErr
		}
		coveredPkgs[filepath.Dir(rel)]++

		// internal/store is *supposed* to contain all of this, including
		// its own test files (which legitimately contain SQL), so the whole
		// package is skipped for both checks.
		if strings.HasPrefix(rel, storeDirPrefix) {
			return nil
		}

		// This file's own forbiddenSQLLiterals slice literally spells out
		// every keyword fragment below as a Go string constant, so an
		// unconditional scan would flag its own definition list. Excluding
		// this one file (not the whole cmd/keep package; init.go and main.go
		// are still fully scanned) keeps the rule from tripping on its own
		// definition.
		if path == thisFile {
			return checkImports(t, fset, path, rel)
		}

		if err := checkImports(t, fset, path, rel); err != nil {
			return err
		}
		return checkSQLLiterals(t, fset, path, rel)
	})
	if walkErr != nil {
		t.Fatalf("walking module tree: %v", walkErr)
	}

	// A walk that finds nothing must fail, or a repository-scanning test can
	// quietly become a permanent pass.
	if filesWalked == 0 {
		t.Fatal("walked zero .go files -- the walk root is broken; treat this as a failure, not a vacuous pass")
	}

	// The walk root reaches every package by construction, so this loop
	// should never fire, which is why it is here: a walk that silently stops
	// reaching a package would pass forever. Every package falls inside the
	// walk as soon as its directory exists; naming them explicitly turns "it
	// happens to be covered" into "it is required to be covered", so a walk
	// whose reach shrinks fails by name.
	for _, pkg := range []string{"cmd/keep", "internal/mcp", "internal/model", "internal/dash", "internal/registry"} {
		if coveredPkgs[filepath.FromSlash(pkg)] == 0 {
			t.Errorf("SQL-boundary walk visited zero .go files in %s -- the walk's reach has shrunk", pkg)
		}
	}
}

func checkImports(t *testing.T, fset *token.FileSet, path, rel string) error {
	t.Helper()
	f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	if err != nil {
		return fmt.Errorf("parse %s (imports): %w", rel, err)
	}
	for _, imp := range f.Imports {
		importPath := strings.Trim(imp.Path.Value, `"`)
		for _, forbidden := range forbiddenImports {
			if importPath == forbidden {
				t.Errorf("SQL boundary violation: %s imports %q -- internal/store is the only package containing SQL, so nothing else may import a SQL driver package", rel, importPath)
			}
		}
	}
	return nil
}

func checkSQLLiterals(t *testing.T, fset *token.FileSet, path, rel string) error {
	t.Helper()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return fmt.Errorf("parse %s (full): %w", rel, err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		for _, kw := range forbiddenSQLLiterals {
			if strings.Contains(lit.Value, kw) {
				t.Errorf("SQL boundary violation: %s contains SQL literal %s (matches forbidden fragment %q) -- internal/store is the only package containing SQL", rel, lit.Value, kw)
			}
		}
		return true
	})
	return nil
}
