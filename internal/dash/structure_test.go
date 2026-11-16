package dash

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// forbiddenTemplateTrustCalls are template.* constructors that fully defeat
// html/template's contextual escaping. Wrapping agent-supplied data in any
// of these tells the template engine to trust the string literally, which
// the rendered-output scan depends on never happening.
// template.CSS is deliberately absent from this set: it has its own,
// narrower check below (allowed at most once, and only in one documented
// file).
var forbiddenTemplateTrustCalls = map[string]bool{
	"HTML":     true,
	"HTMLAttr": true,
	"URL":      true,
	"JS":       true,
}

// allowedCSSFile is the one file documented as template.CSS's exception, for
// the reason templates.go's comment on styleInline gives: styleCSS is a
// compile-time go:embed of a static stylesheet living in this repository,
// not agent data, and cannot become agent data (no code in this package
// ever writes to it), so it is reviewable once by a human rather than
// scanned per render. Inlining the CSS differently means updating this
// constant and the "at most 1" assertion below, not widening the exception.
const allowedCSSFile = "templates.go"

// forbiddenDashImports mirrors cmd/keep/boundary_test.go's own list for
// the two entries relevant here. TestNoSQLAboveStore already enforces this
// module-wide (and its coverage assertion proves it reaches
// internal/dash); asserting it again HERE, locally, means a failure
// names this package directly rather than requiring a reader to trust that
// the module-wide walk's reach has not silently shrunk.
var forbiddenDashImports = []string{"database/sql", "modernc.org/sqlite", "modernc.org/sqlite/lib"}

// TestDashHasNoStoreBehaviour checks on the AST that internal/dash carries
// no behaviour and no trust types of its own, so it stays a thin caller of
// internal/store. It follows internal/mcp/structure_test.go's technique
// (TestNoConditionalInToolHandlers): parse the package's non-test .go files
// with go/parser, walk with ast.Inspect, and assert on AST shapes rather
// than grep output, which flags a doc-comment mention of a forbidden
// pattern as readily as a real one.
func TestDashHasNoStoreBehaviour(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed -- cannot locate this test's own file to derive internal/dash's directory")
	}
	dir := filepath.Dir(thisFile)

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parser.ParseDir(%s): %v", dir, err)
	}

	type fileEntry struct {
		name string
		file *ast.File
	}
	var files []fileEntry
	for _, pkg := range pkgs {
		for name, f := range pkg.Files {
			files = append(files, fileEntry{filepath.Base(name), f})
		}
	}
	// A walk that finds nothing must fail, as in TestNoSQLAboveStore and
	// internal/mcp's TestNoConditionalInToolHandlers, so a
	// repository-scanning test cannot become a vacuous pass.
	if len(files) == 0 {
		t.Fatal("walked zero non-test .go files in internal/dash -- the walk is broken; treat this as a failure, not a vacuous pass")
	}

	cssCount := 0
	var cssFiles []string

	for _, fe := range files {
		for _, imp := range fe.file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, forbidden := range forbiddenDashImports {
				if path == forbidden {
					t.Errorf("internal/dash/%s imports %q -- internal/dash must never import a SQL driver directly", fe.name, path)
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
					t.Errorf("internal/dash/%s:%d calls template.%s(...) -- this fully defeats html/template's contextual escaping; the off-host guarantee depends on this never happening", fe.name, pos.Line, sel.Sel.Name)
				}
				if sel.Sel.Name == "CSS" {
					cssCount++
					cssFiles = append(cssFiles, fe.name)
				}
			case *ast.Ident:
				if node.Name == "withTx" {
					pos := fset.Position(node.Pos())
					t.Errorf("internal/dash/%s:%d references withTx -- internal/dash must never hold a transaction; every existing store read method already opens and closes its own connection use per call", fe.name, pos.Line)
				}
			}
			return true
		})
	}

	// template.CSS is the one documented exception (styleInline,
	// templates.go): a compile-time go:embed of a static file, not agent
	// data. Assert the count is at most 1 and name the file it is allowed
	// in, rather than a blanket zero. If the stylesheet is inlined
	// differently (0 occurrences), this assertion still holds, since "at
	// most 1" permits zero.
	if cssCount > 1 {
		t.Errorf("template.CSS(...) called %d times across %v, want at most 1 -- the one documented exception is templates.go's styleInline", cssCount, cssFiles)
	}
	for _, f := range cssFiles {
		if f != allowedCSSFile {
			t.Errorf("template.CSS(...) called in %q, want it confined to %q (the one documented exception)", f, allowedCSSFile)
		}
	}
}
