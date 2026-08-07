package mcp

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

// wantHandlerCount is the number of MCP tools this package registers
// (04-CONTEXT.md decision 2). Fewer than this must fail loudly -- a broken
// matcher (or a broken registration path, see GATE-03 break 1 in this
// plan's verification section) must not pass vacuously.
const wantHandlerCount = 10

// TestNoConditionalInToolHandlers is the AST proof of MCP-05 --
// docs/design/phase-3.md line 63's "no tool handler contains a conditional",
// narrowed by 04-CONTEXT.md decision 4 to "no conditional on business
// state; the single errors.Is switch mapping store sentinels to results is
// permitted, lives in one shared helper, and is the only branch in the
// package." phase-3.md says this rule is "checked by review" -- this test
// replaces that review with a mechanical walk, following the same
// go/ast-over-go/types-free precedent cmd/rounce/boundary_test.go's
// TestNoSQLAboveStore and internal/store/mutating_scan_test.go's
// mutatingStoreMethods already established in this codebase (grep would
// not distinguish the permitted switch from business branching; an AST
// walk states the rule precisely enough that nobody has to interpret it).
func TestNoConditionalInToolHandlers(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed -- cannot locate this test's own file to derive internal/mcp's directory")
	}
	dir := filepath.Dir(thisFile)

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parser.ParseDir(%s): %v", dir, err)
	}

	var files []*ast.File
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			files = append(files, f)
		}
	}
	// A walk that finds nothing must fail -- this is the single most common
	// way a repository-scanning test rots into a permanent pass (same
	// discipline TestNoSQLAboveStore and mutatingStoreMethods follow).
	if len(files) == 0 {
		t.Fatal("walked zero non-test .go files in internal/mcp -- the walk is broken; treat this as a failure, not a vacuous pass")
	}

	// storeParamName is the identifier bound to internal/mcp's one
	// *store.Store parameter (NewServer's "st") -- discovered from the AST
	// rather than hard-coded, so a rename cannot silently defeat the
	// "exactly one store call per handler" check below.
	storeParamName := findStoreParamName(t, files)

	handlers := findToolHandlers(files)
	if len(handlers) != wantHandlerCount {
		t.Fatalf("found %d tool-handler function literals, want exactly %d -- either the matcher is broken or the registration path changed (see this plan's GATE-03 break 1, which reproduces exactly this failure by registering one tool through the SDK's generic AddTool[In,Out] instead of the low-level path)", len(handlers), wantHandlerCount)
	}

	for _, h := range handlers {
		checkHandlerShape(t, fset, h, storeParamName)
	}

	checkOnlyBranchOutsideHandlers(t, fset, files, handlers)
}

// findToolHandlers returns every *ast.FuncLit in files whose type matches
// mcp.ToolHandler's shape: func(context.Context, *mcp.CallToolRequest)
// (*mcp.CallToolResult, error). Matching on the parameter/result type
// expressions -- not on "second argument to s.AddTool" -- so this keeps
// finding the handlers even if registration is ever refactored into a
// helper function.
func findToolHandlers(files []*ast.File) []*ast.FuncLit {
	var out []*ast.FuncLit
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.FuncLit)
			if ok && isToolHandlerType(lit.Type) {
				out = append(out, lit)
			}
			return true
		})
	}
	return out
}

func isToolHandlerType(ft *ast.FuncType) bool {
	if ft.Params == nil || len(ft.Params.List) != 2 {
		return false
	}
	if ft.Results == nil || len(ft.Results.List) != 2 {
		return false
	}
	return isSelectorType(ft.Params.List[0].Type, "context", "Context") &&
		isStarSelectorType(ft.Params.List[1].Type, "mcp", "CallToolRequest") &&
		isStarSelectorType(ft.Results.List[0].Type, "mcp", "CallToolResult") &&
		isIdentType(ft.Results.List[1].Type, "error")
}

func isSelectorType(expr ast.Expr, pkg, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg && sel.Sel.Name == name
}

func isStarSelectorType(expr ast.Expr, pkg, name string) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	return isSelectorType(star.X, pkg, name)
}

func isIdentType(expr ast.Expr, name string) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == name
}

// findStoreParamName locates the exactly-one top-level function parameter
// of type *store.Store in internal/mcp's own (non-test) source and returns
// the identifier it is bound to. Failing loudly (rather than defaulting to
// a hard-coded "st") if that assumption -- exactly one such parameter,
// belonging to NewServer -- ever stops holding.
func findStoreParamName(t *testing.T, files []*ast.File) string {
	t.Helper()
	var name string
	count := 0
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Type.Params == nil {
				continue
			}
			for _, field := range fn.Type.Params.List {
				if isStarSelectorType(field.Type, "store", "Store") {
					for _, id := range field.Names {
						name = id.Name
						count++
					}
				}
			}
		}
	}
	if count != 1 {
		t.Fatalf("findStoreParamName: found %d *store.Store parameters across internal/mcp's top-level functions, want exactly 1 (NewServer's) -- cannot determine which identifier handler bodies' store calls are made through", count)
	}
	return name
}

// checkHandlerShape enforces MCP-05's per-handler property: exactly one if
// statement (the json.Unmarshal error check), no switch/type-switch/for/
// range/select, and exactly one call to an exported method on the
// *store.Store parameter.
func checkHandlerShape(t *testing.T, fset *token.FileSet, lit *ast.FuncLit, storeParamName string) {
	pos := fset.Position(lit.Pos())

	var ifStmts []*ast.IfStmt
	var forbidden []ast.Node
	storeCalls := 0

	ast.Inspect(lit.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.IfStmt:
			ifStmts = append(ifStmts, node)
		case *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SelectStmt:
			forbidden = append(forbidden, n)
		case *ast.CallExpr:
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == storeParamName && ast.IsExported(sel.Sel.Name) {
					storeCalls++
				}
			}
		}
		return true
	})

	if len(forbidden) > 0 {
		fpos := fset.Position(forbidden[0].Pos())
		t.Errorf("tool handler at %s:%d contains a %T at %s -- MCP-05 forbids a loop or a second branch in a handler; if this logic is needed, it belongs in internal/store where the CLI reaches it too (docs/design/phase-3.md)", pos.Filename, pos.Line, forbidden[0], fpos)
	}

	if len(ifStmts) != 1 {
		t.Errorf("tool handler at %s:%d contains %d if statements, want exactly 1 (the json.Unmarshal error check). MCP-05 forbids a conditional on business state in a handler -- if this branch is needed, it belongs in internal/store where the CLI reaches it too (docs/design/phase-3.md). If it is error mapping, it belongs in resultFor (internal/mcp/errors.go), which is the one shared switch 04-CONTEXT.md decision 4 permits.", pos.Filename, pos.Line, len(ifStmts))
	} else if !isUnmarshalErrorCheck(ifStmts[0]) {
		t.Errorf("tool handler at %s:%d has exactly one if statement, but its shape is not `if err := json.Unmarshal(...); err != nil` -- MCP-05 only permits this one conditional per handler", pos.Filename, pos.Line)
	}

	if storeCalls != 1 {
		t.Errorf("tool handler at %s:%d calls %d exported method(s) on %q (the *store.Store parameter), want exactly 1 -- MCP-05 requires one store call per handler, its result passed straight to resultFor", pos.Filename, pos.Line, storeCalls, storeParamName)
	}
}

// isUnmarshalErrorCheck reports whether stmt is exactly
// `if err := json.Unmarshal(...); err != nil { ... }` -- the one
// conditional MCP-05 permits per handler.
func isUnmarshalErrorCheck(stmt *ast.IfStmt) bool {
	assign, ok := stmt.Init.(*ast.AssignStmt)
	if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		return false
	}
	errIdent, ok := assign.Lhs[0].(*ast.Ident)
	if !ok {
		return false
	}
	call, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok || pkgIdent.Name != "json" || sel.Sel.Name != "Unmarshal" {
		return false
	}

	bin, ok := stmt.Cond.(*ast.BinaryExpr)
	if !ok || bin.Op != token.NEQ {
		return false
	}
	condIdent, ok := bin.X.(*ast.Ident)
	if !ok || condIdent.Name != errIdent.Name {
		return false
	}
	nilIdent, ok := bin.Y.(*ast.Ident)
	return ok && nilIdent.Name == "nil"
}

// ifOutsideHandler names one *ast.IfStmt found outside every tool handler,
// together with the enclosing top-level function's name, so a failure names
// exactly where an unexpected conditional appeared.
type ifOutsideHandler struct {
	node *ast.IfStmt
	fn   string
}

// checkOnlyBranchOutsideHandlers enforces the package-wide half of MCP-05:
// exactly one *ast.SwitchStmt in the whole package (resultFor's sentinel
// classification), and exactly two *ast.IfStmt outside every tool handler
// (NewServer's schema-error check, and resultFor's err == nil success
// check) -- named by enclosing function, so a new one anywhere else fails
// by name, not just by count.
func checkOnlyBranchOutsideHandlers(t *testing.T, fset *token.FileSet, files []*ast.File, handlers []*ast.FuncLit) {
	handlerSet := make(map[*ast.FuncLit]bool, len(handlers))
	for _, h := range handlers {
		handlerSet[h] = true
	}

	var switches []struct {
		node *ast.SwitchStmt
		fn   string
	}
	var ifs []ifOutsideHandler

	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if lit, ok := n.(*ast.FuncLit); ok && handlerSet[lit] {
					// Already fully checked by checkHandlerShape --
					// do not descend, so its own if/switch/etc. is
					// not double-counted here as "outside a handler".
					return false
				}
				switch node := n.(type) {
				case *ast.SwitchStmt:
					switches = append(switches, struct {
						node *ast.SwitchStmt
						fn   string
					}{node, fn.Name.Name})
				case *ast.IfStmt:
					ifs = append(ifs, ifOutsideHandler{node, fn.Name.Name})
				}
				return true
			})
		}
	}

	if len(switches) != 1 {
		var locs []string
		for _, s := range switches {
			locs = append(locs, fmt.Sprintf("%s (in %s)", fset.Position(s.node.Pos()), s.fn))
		}
		t.Fatalf("package contains %d *ast.SwitchStmt, want exactly 1 (resultFor's sentinel-classification switch, 04-CONTEXT.md decision 4's only permitted branch): %v", len(switches), locs)
	} else if switches[0].fn != "resultFor" {
		t.Errorf("the package's one *ast.SwitchStmt is in %q at %s, want it inside resultFor (internal/mcp/errors.go)", switches[0].fn, fset.Position(switches[0].node.Pos()))
	}

	wantIfFuncs := map[string]bool{"NewServer": true, "resultFor": true}
	if len(ifs) != len(wantIfFuncs) {
		var locs []string
		for _, e := range ifs {
			locs = append(locs, fmt.Sprintf("%s (in %s)", fset.Position(e.node.Pos()), e.fn))
		}
		t.Fatalf("package contains %d *ast.IfStmt outside tool handlers, want exactly %d (NewServer's schema-error check and resultFor's err == nil success check): %v", len(ifs), len(wantIfFuncs), locs)
	}
	seen := map[string]bool{}
	for _, e := range ifs {
		seen[e.fn] = true
		if !wantIfFuncs[e.fn] {
			t.Errorf("unexpected *ast.IfStmt outside a tool handler in function %q at %s -- want only NewServer and resultFor", e.fn, fset.Position(e.node.Pos()))
		}
	}
	for fn := range wantIfFuncs {
		if !seen[fn] {
			t.Errorf("expected an *ast.IfStmt in function %q, found none", fn)
		}
	}
}
