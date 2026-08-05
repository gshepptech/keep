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
// internal/store direct access to a SQL driver -- exactly what STORE-10
// forbids: "Nothing above internal/store ever sees a *sql.DB, a row, or a
// query string."
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

// TestNoSQLAboveStore mechanically enforces STORE-10 -- "internal/store is
// the only package containing SQL" -- rather than relying on doc.go comments
// and code review. It parses every non-vendored .go file in the module and
// fails if any file outside internal/store imports a SQL driver package or
// contains a SQL keyword string literal.
func TestNoSQLAboveStore(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed -- cannot locate this test's own file to derive the module root")
	}

	// This file lives at <module root>/cmd/rounce/boundary_test.go, so two
	// levels up is the module root -- resilient to the package being run
	// from a different working directory, and it fails loudly (via the
	// zero-file guard below) if the module layout ever changes rather than
	// silently scanning nothing.
	moduleRoot, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}

	fset := token.NewFileSet()
	filesWalked := 0

	walkErr := filepath.WalkDir(moduleRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// A broken walk root (e.g. moduleRoot itself does not exist)
			// surfaces here with a nil DirEntry. Do not propagate that as a
			// hard failure -- let the walk finish having found nothing, so
			// the filesWalked == 0 guard below is the mechanism that turns
			// "found nothing" into a failure, not an incidental I/O error.
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".planning", "testdata":
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

		// internal/store is *supposed* to contain all of this, including
		// its own test files (which legitimately contain SQL) -- skip the
		// whole package for both checks.
		if strings.HasPrefix(rel, storeDirPrefix) {
			return nil
		}

		// This file's own forbiddenSQLLiterals slice literally spells out
		// every keyword fragment below as a Go string constant -- an
		// unconditional scan would flag its own definition list. Excluding
		// this one file (not the whole cmd/rounce package -- init.go and
		// main.go are still fully scanned) is the only way to define the
		// rule without the rule tripping on its own definition.
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

	// A walk that finds nothing must fail -- this is the single most common
	// way a repository-scanning test rots into a permanent pass.
	if filesWalked == 0 {
		t.Fatal("walked zero .go files -- the walk root is broken; treat this as a failure, not a vacuous pass")
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
				t.Errorf("STORE-10 violation: %s imports %q -- internal/store is the only package containing SQL, so nothing else may import a SQL driver package", rel, importPath)
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
				t.Errorf("STORE-10 violation: %s contains SQL literal %s (matches forbidden fragment %q) -- internal/store is the only package containing SQL", rel, lit.Value, kw)
			}
		}
		return true
	})
	return nil
}
