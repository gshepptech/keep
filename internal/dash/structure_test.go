package dash

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// forbiddenTemplateTrustCalls are template.* constructors that fully defeat
// html/template's contextual escaping. Wrapping agent-supplied data in any
// of these tells the engine to trust the string literally, which the
// rendered-output scan depends on never happening. template.CSS is absent
// from this set because it has its own, narrower check below: at most once,
// and only in one documented file.
var forbiddenTemplateTrustCalls = map[string]bool{
	"HTML":     true,
	"HTMLAttr": true,
	"URL":      true,
	"JS":       true,
}

// allowedCSSFile is the one file allowed to call template.CSS. styleInline
// there wraps styleCSS, a compile-time go:embed of a static stylesheet in
// this repository. It is not agent data and cannot become agent data, so it
// is reviewed once by a person rather than scanned per render. If the CSS
// is ever inlined differently, this constant and the "at most 1" assertion
// below change, not the size of the exception.
const allowedCSSFile = "templates.go"

// forbiddenDashImports mirrors cmd/keep/boundary_test.go's list.
// TestNoSQLAboveStore already enforces it module-wide; checking it here as
// well means a failure names this package directly.
var forbiddenDashImports = []string{"database/sql", "modernc.org/sqlite", "modernc.org/sqlite/lib"}

// TestDashHasNoStoreBehaviour checks that internal/dash is a thin caller of
// internal/store with no SQL driver import, no transaction and no
// html/template trust types. Like internal/mcp's
// TestNoConditionalInToolHandlers, it parses the package's non-test files
// and asserts on AST shapes rather than grep output, which would flag a
// comment that mentions a forbidden pattern as readily as real code.
func TestDashHasNoStoreBehaviour(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed: cannot locate this test's own file to derive internal/dash's directory")
	}
	dir := filepath.Dir(thisFile)

	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}

	type fileEntry struct {
		name string
		file *ast.File
	}
	fset := token.NewFileSet()
	var files []fileEntry
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files = append(files, fileEntry{filepath.Base(path), f})
	}
	// A walk that finds nothing must fail, or the test passes forever
	// without checking anything.
	if len(files) == 0 {
		t.Fatal("walked zero non-test .go files in internal/dash; the walk is broken, and an empty walk must fail rather than pass")
	}

	cssCount := 0
	var cssFiles []string

	for _, fe := range files {
		for _, imp := range fe.file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, forbidden := range forbiddenDashImports {
				if path == forbidden {
					t.Errorf("internal/dash/%s imports %q; internal/dash must never import a SQL driver directly", fe.name, path)
				}
			}
		}

		ast.Inspect(fe.file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkgIdent, ok := sel.X.(*ast.Ident)
				if !ok || pkgIdent.Name != "template" {
					return true
				}
				if forbiddenTemplateTrustCalls[sel.Sel.Name] {
					pos := fset.Position(node.Pos())
					t.Errorf("internal/dash/%s:%d calls template.%s(...), which fully defeats html/template's contextual escaping; the off-host guarantee depends on this never happening", fe.name, pos.Line, sel.Sel.Name)
				}
				if sel.Sel.Name == "CSS" {
					cssCount++
					cssFiles = append(cssFiles, fe.name)
				}
			case *ast.Ident:
				if node.Name == "withTx" {
					pos := fset.Position(node.Pos())
					t.Errorf("internal/dash/%s:%d references withTx; internal/dash must never hold a transaction, because every store read method opens and closes its own connection use per call", fe.name, pos.Line)
				}
			}
			return true
		})
	}

	// template.CSS is allowed at most once, and only in allowedCSSFile.
	// "At most" rather than "exactly" so that inlining the stylesheet
	// some other way does not fail this test.
	if cssCount > 1 {
		t.Errorf("template.CSS(...) called %d times across %v, want at most 1; the one documented exception is templates.go's styleInline", cssCount, cssFiles)
	}
	for _, f := range cssFiles {
		if f != allowedCSSFile {
			t.Errorf("template.CSS(...) called in %q, want it confined to %q (the one documented exception)", f, allowedCSSFile)
		}
	}
}
