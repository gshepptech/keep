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

// mutatingStoreMethods walks every non-test .go file in this package's own
// directory and returns the set of exported *Store methods that mutate the
// database -- directly, or transitively through any same-package function or
// method they call.
//
// Why name-based call matching, not go/types: this package is eight files
// with no name collisions across receivers, and cmd/keep/boundary_test.go
// already proves the same "match ast.Ident / ast.SelectorExpr.Sel.Name
// against a keyword" idiom works in this codebase without a full type
// checker. A fully type-checked call graph (go/types + x/tools/go/packages)
// would correctly resolve calls through interfaces, which this package does
// not have any of -- that precision would be unjustified weight for an
// 8-file package with a single concrete receiver type. See 03-RESEARCH.md
// "Alternatives Considered". Do not "upgrade" this later without a concrete
// case name-based matching gets wrong.
//
// Why test files are excluded: _test.go files legitimately contain SQL
// fixtures of their own (this file's own fixtureItem in events_test.go,
// testhelp_test.go's execSQL) that call INSERT/UPDATE/DELETE directly for
// test setup. Including them would make every test helper that happens to
// write SQL look like a "mutating store method", which is not what EVENT-01
// is enforcing.
//
// Why transitive matters: a per-method (own-body-only) scan finds only 2 of
// this package's 6 mutating methods -- StartItem and UpdateItem, whose
// UPDATE items SET literals sit inside the closure passed to s.withTx, a
// nested *ast.FuncLit within the same top-level FuncDecl that ast.Inspect
// recurses into. It MISSES AddItem, CreateDep, CloseItem, and DropItem,
// which delegate their SQL to shared helpers (insertItem, insertEdge,
// insertParentChildEdge, writeEvent, closeOrDrop). A scan reporting 2 is
// worse than one reporting 0: it is non-empty, passes any "did we find
// anything" smoke check, and reads as though EVENT-01 were structurally
// enforced while actually covering a third of what it claims to
// (03-RESEARCH.md Pitfall 2). This is why callers of this function must
// additionally guard on "fewer than six", not "zero" or "non-empty" --
// see TestMutatingScanClassifiesKnownMethods below.
func mutatingStoreMethods(t *testing.T) map[string]bool {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed -- cannot locate this test's own file to derive internal/store's directory")
	}
	dir := filepath.Dir(thisFile)

	fset := token.NewFileSet()

	// directMutating[funcName] = true if that function/method's own body
	// (including nested function literals, e.g. the closure passed to
	// s.withTx) contains a BasicLit STRING matching one of the three
	// mutating SQL keyword fragments.
	directMutating := map[string]bool{}
	// callGraph[funcName] = set of same-package function/method names called
	// from that function's body (transitively followed below).
	callGraph := map[string]map[string]bool{}
	// methodReceiver[funcName] = the receiver type name, for funcs with a
	// receiver -- used to filter to *Store methods only.
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
		t.Fatal("mutatingStoreMethods: walked zero .go files in internal/store -- the directory layout is broken; treat this as a failure, not a vacuous pass")
	}

	// reaches(name) = directMutating[name] || any(reaches(callee) for callee
	// in callGraph[name]), memoized with a visiting-set cycle guard so a
	// mutual-recursion cycle in the call graph cannot loop forever.
	memo := map[string]bool{}
	visiting := map[string]bool{}
	var reaches func(name string) bool
	reaches = func(name string) bool {
		if v, ok := memo[name]; ok {
			return v
		}
		if visiting[name] {
			// Already on the stack -- this call contributes nothing new;
			// let the frame that started the cycle finish on its own
			// direct/other-callee evidence.
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

// receiverTypeName extracts the bare type name from a receiver's type
// expression, unwrapping the pointer if present -- func (s *Store) ... and
// func (s Store) ... both yield "Store".
func receiverTypeName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// mutatingSQLFragments are the same three keyword fragments
// cmd/keep/boundary_test.go's forbiddenSQLLiterals uses for SELECT/INSERT/
// UPDATE/DELETE/CREATE/PRAGMA -- narrowed here to the three that indicate a
// WRITE, since SELECT/CREATE TABLE/PRAGMA appear in this package's read
// paths and schema setup without indicating a mutating *Store method.
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

// wantMutating and wantReadOnly are today's known classification, verified
// during 03-RESEARCH.md against this exact package (Pattern 3) and re-
// confirmed here on every run. Listed both directions deliberately: an
// over-broad scan that returned every method would satisfy EVENT-01's
// set-equality guard (events_test.go) only by demanding rows for read-only
// methods too, and that would be noticed here as unexpected noise rather
// than silently passing task 2's guard.
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
	}
)

// TestMutatingScanClassifiesKnownMethods is this scan's self-check: it must
// reproduce today's exact 6/6 split against the real, unmodified package,
// and it must fail loudly rather than silently passing if the scan came back
// broken.
func TestMutatingScanClassifiesKnownMethods(t *testing.T) {
	got := mutatingStoreMethods(t)

	// The fewer-than-six guard, as its own explicit failure: a scan
	// reporting FEWER than six mutating methods against the unmodified
	// package is the signature of the non-transitive bug (03-RESEARCH.md
	// Pitfall 2), which reports 2 -- not 0. A "found nothing" / "found
	// something" check would NOT catch a scan stuck at 2, so the guard here
	// is explicitly a count threshold, not an emptiness check.
	if len(got) < len(wantMutating) {
		t.Fatalf("mutatingStoreMethods found only %d mutating methods (%v), want at least %d -- this is the non-transitive-scan bug (03-RESEARCH.md Pitfall 2): a shallow per-method scan finds StartItem and UpdateItem (literal sits in their own withTx closure) but misses AddItem, CreateDep, CloseItem, and DropItem (SQL delegated to a helper) -- it reports 2, not 0, so this is a code change, not an unmodified-package fluke", len(got), setKeys(got), len(wantMutating))
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
		t.Errorf("mutatingStoreMethods returned %d methods %v, want exactly the %d in wantMutating %v -- scan found an unexpected extra method", len(got), setKeys(got), len(wantMutating), setKeys(wantMutating))
	}
}

func setKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
