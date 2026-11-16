package store

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

// mutatingStoreMethods walks every non-test .go file in this package's
// directory and returns the exported *Store methods that write to the
// database, either directly or through any same-package function they call.
//
// Calls are matched by name rather than through go/types. This package has
// one concrete receiver type, no interfaces and no name collisions across
// receivers, so a type-checked call graph would add weight without changing
// the answer.
//
// Test files are excluded because their fixtures and helpers run INSERT,
// UPDATE and DELETE for setup, and the one-event-per-mutation rule is about
// store methods, not test setup.
//
// The walk is transitive because most mutating methods delegate their SQL
// to shared helpers (insertItem, insertEdge, insertParentChildEdge,
// writeEvent, closeOrDrop). A scan of each method's own body would find
// only StartItem and UpdateItem, whose UPDATE literals sit in the closure
// passed to s.withTx. That result is non-empty, so a "found something"
// check would pass it. TestMutatingScanClassifiesKnownMethods guards on the
// full expected set instead.
func mutatingStoreMethods(t *testing.T) map[string]bool {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed: cannot locate this test's own file to derive internal/store's directory")
	}
	dir := filepath.Dir(thisFile)

	fset := token.NewFileSet()

	// directMutating[funcName] is true when that function's own body,
	// including nested function literals such as the closure passed to
	// s.withTx, holds a string literal with a mutating SQL fragment.
	directMutating := map[string]bool{}
	// callGraph[funcName] is the set of same-package names called from that
	// function's body, followed transitively below.
	callGraph := map[string]map[string]bool{}
	// methodReceiver[funcName] is the receiver type name, used to keep only
	// *Store methods.
	methodReceiver := map[string]string{}

	filesWalked := 0

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("mutatingStoreMethods: ReadDir(%s): %v", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("mutatingStoreMethods: parse %s: %v", path, err)
		}
		filesWalked++

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			funcName := fn.Name.Name

			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				methodReceiver[funcName] = receiverTypeName(fn.Recv.List[0].Type)
			}

			calls := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.BasicLit:
					if node.Kind == token.STRING && containsMutatingSQL(node.Value) {
						directMutating[funcName] = true
					}
				case *ast.CallExpr:
					switch fun := node.Fun.(type) {
					case *ast.Ident:
						calls[fun.Name] = true
					case *ast.SelectorExpr:
						calls[fun.Sel.Name] = true
					}
				}
				return true
			})
			callGraph[funcName] = calls
		}
	}

	if filesWalked == 0 {
		t.Fatal("mutatingStoreMethods: walked zero .go files in internal/store; the directory layout is broken, and an empty walk must fail rather than pass")
	}

	// reaches reports whether name, or anything it calls, writes. It is
	// memoized, and the visiting set stops a recursive cycle in the call
	// graph from looping forever.
	memo := map[string]bool{}
	visiting := map[string]bool{}
	var reaches func(name string) bool
	reaches = func(name string) bool {
		if v, ok := memo[name]; ok {
			return v
		}
		if visiting[name] {
			// Already on the stack. This call adds nothing new; the frame
			// that started the cycle decides on its own evidence.
			return false
		}
		visiting[name] = true
		defer delete(visiting, name)

		result := directMutating[name]
		if !result {
			for callee := range callGraph[name] {
				if reaches(callee) {
					result = true
					break
				}
			}
		}
		memo[name] = result
		return result
	}

	mutating := map[string]bool{}
	for name, recv := range methodReceiver {
		if recv != "Store" {
			continue
		}
		if !ast.IsExported(name) {
			continue
		}
		if reaches(name) {
			mutating[name] = true
		}
	}
	return mutating
}

// receiverTypeName returns the bare type name of a receiver, unwrapping a
// pointer, so func (s *Store) and func (s Store) both yield "Store".
func receiverTypeName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// mutatingSQLFragments is the subset of cmd/keep/boundary_test.go's
// forbiddenSQLLiterals that indicates a write. SELECT, CREATE TABLE and
// PRAGMA appear in read paths and schema setup, so they are left out.
var mutatingSQLFragments = []string{
	"INSERT INTO",
	"UPDATE ",
	"DELETE FROM",
}

func containsMutatingSQL(literal string) bool {
	for _, frag := range mutatingSQLFragments {
		if strings.Contains(literal, frag) {
			return true
		}
	}
	return false
}

// wantMutating and wantReadOnly are the expected classification. Both
// directions are listed so that an over-broad scan, one that reported every
// method as mutating, fails here by name instead of only surfacing as extra
// rows demanded by events_test.go's one-event table.
var (
	wantMutating = map[string]bool{
		"AddItem":     true,
		"StartItem":   true,
		"UnclaimItem": true,
		"ReopenItem":  true,
		"CloseItem":   true,
		"DropItem":    true,
		"UpdateItem":  true,
		"CreateDep":   true,
		"NextItem":    true,
		"Import":      true,
	}
	wantReadOnly = map[string]bool{
		"Ready":        true,
		"Blocked":      true,
		"GetItem":      true,
		"ListItems":    true,
		"IsBlocked":    true,
		"Close":        true,
		"Backlog":      true,
		"Tree":         true,
		"LastActivity": true,
		"WhyNot":       true,
		"Export":       true,
	}
)

// TestMutatingScanClassifiesKnownMethods checks that the scan reproduces the
// expected classification of every exported *Store method against the real
// package.
func TestMutatingScanClassifiesKnownMethods(t *testing.T) {
	got := mutatingStoreMethods(t)

	// A count threshold rather than an emptiness check: a scan that stops
	// following calls still finds StartItem and UpdateItem, so it is never
	// empty, only short.
	if len(got) < len(wantMutating) {
		t.Fatalf("mutatingStoreMethods found only %d mutating methods (%v), want at least %d; the scan is no longer following calls into the helpers that hold the SQL", len(got), setKeys(got), len(wantMutating))
	}

	for name := range wantMutating {
		if !got[name] {
			t.Errorf("mutatingStoreMethods: expected %q to be classified as mutating, but it was not found", name)
		}
	}
	for name := range wantReadOnly {
		if got[name] {
			t.Errorf("mutatingStoreMethods: expected %q to be classified as read-only, but the scan reported it as mutating", name)
		}
	}

	if len(got) != len(wantMutating) {
		t.Errorf("mutatingStoreMethods returned %d methods %v, want exactly the %d in wantMutating %v; the scan found an unexpected extra method", len(got), setKeys(got), len(wantMutating), setKeys(wantMutating))
	}
}

func setKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
