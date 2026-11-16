package mcp

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// wantHandlerCount is the number of MCP tools this package registers,
// derived from ToolNames rather than typed again, so a broken matcher or a
// tool registered through a different path fails instead of passing with
// nothing checked.
//
// The schema test shares this source but checks something else: it reads
// the map BuildSchemas returns, while this test counts function literals in
// the parsed AST. A tool in ToolNames with a schema and no handler fails
// here and passes there.
var wantHandlerCount = len(ToolNames)

// TestNoConditionalInToolHandlers checks the handler rule: no tool handler
// branches on business state, and the one switch that maps store sentinels
// to results lives in a shared helper and is the only switch in the
// package. It walks the AST, as cmd/keep's TestNoSQLAboveStore and
// internal/store's mutatingStoreMethods do, because grep cannot tell the
// permitted switch from business branching.
func TestNoConditionalInToolHandlers(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed: cannot locate this test's own file to derive internal/mcp's directory")
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
	// A walk that finds nothing must fail, or the test passes forever
	// without checking anything.
	if len(files) == 0 {
		t.Fatal("walked zero non-test .go files in internal/mcp; the walk is broken, and an empty walk must fail rather than pass")
	}

	// storeParamName is the identifier bound to internal/mcp's one
	// *store.Store parameter (NewServer's "st"). It is read from the AST
	// rather than hard-coded, so a rename cannot silently defeat the
	// "exactly one store call per handler" check below.
	storeParamName := findStoreParamName(t, files)

	handlers := findToolHandlers(files)
	if len(handlers) != wantHandlerCount {
		t.Fatalf("found %d tool-handler function literals, want exactly %d; either the matcher is broken or the registration path changed (registering a tool through the SDK's generic AddTool[In,Out] instead of the low-level path produces this failure)", len(handlers), wantHandlerCount)
	}

	for _, h := range handlers {
		checkHandlerShape(t, fset, h, storeParamName)
	}

	checkOnlyBranchOutsideHandlers(t, fset, files, handlers)
}

// findToolHandlers returns every *ast.FuncLit in files whose type matches
// mcp.ToolHandler's shape: func(context.Context, *mcp.CallToolRequest)
// (*mcp.CallToolResult, error). It matches on the parameter and result
// types, not on "second argument to s.AddTool", so it keeps finding the
// handlers if registration moves into a helper function.
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
// of type *store.Store in internal/mcp's non-test source and returns the
// identifier it is bound to. If there is not exactly one such parameter,
// NewServer's, it fails rather than falling back to a hard-coded "st".
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
		t.Fatalf("findStoreParamName: found %d *store.Store parameters across internal/mcp's top-level functions, want exactly 1 (NewServer's); cannot determine which identifier handler bodies' store calls are made through", count)
	}
	return name
}

// injectedReaders names the local identifiers a handler may call INSTEAD of
// a *store.Store method, and there is exactly one.
//
// The handler rule is "exactly one exported call on the store parameter",
// which fits every tool that reads or writes this store. The `ready_all`
// tool does not: the cross-repository queue needs the machine's registry,
// opens every other store read-only, and reports which of them it could not
// read. Importing internal/registry here would put a fan-out inside the
// package that is meant to hold no logic, so cmd/keep injects it as
// Options.ReadyAll and NewServer binds it to this local.
//
// Each handler still makes one data call and passes its result straight to
// resultFor. The call may go to one named, injected reader instead of the
// store. The exception is by name rather than a looser count, so a second
// entry here is a decision someone has to write down.
//
// opts.ActorFor is not in this list and must not be. Every mutating handler
// calls it to resolve who is asking, which is not a data read. Counting it
// would let a handler make a store call and a second call to something else
// while still totalling one.
var injectedReaders = map[string]bool{
	"readyAll": true,
}

// injectedReaderNames renders injectedReaders for a failure message, sorted
// so the message is identical between runs.
func injectedReaderNames() []string {
	out := make([]string, 0, len(injectedReaders))
	for name := range injectedReaders {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// checkHandlerShape enforces the per-handler property: exactly one if
// statement (the json.Unmarshal error check), no switch/type-switch/for/
// range/select, and exactly one data call, either an exported method on
// the *store.Store parameter or one of injectedReaders.
func checkHandlerShape(t *testing.T, fset *token.FileSet, lit *ast.FuncLit, storeParamName string) {
	pos := fset.Position(lit.Pos())

	var ifStmts []*ast.IfStmt
	var forbidden []ast.Node
	storeCalls := 0
	injectedCalls := 0

	ast.Inspect(lit.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.IfStmt:
			ifStmts = append(ifStmts, node)
		case *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SelectStmt:
			forbidden = append(forbidden, n)
		case *ast.CallExpr:
			switch fun := node.Fun.(type) {
			case *ast.SelectorExpr:
				if id, ok := fun.X.(*ast.Ident); ok && id.Name == storeParamName && ast.IsExported(fun.Sel.Name) {
					storeCalls++
				}
			case *ast.Ident:
				if injectedReaders[fun.Name] {
					injectedCalls++
				}
			}
		}
		return true
	})

	if len(forbidden) > 0 {
		fpos := fset.Position(forbidden[0].Pos())
		t.Errorf("tool handler at %s:%d contains a %T at %s; a handler may not hold a loop or a second branch; if this logic is needed, it belongs in internal/store where the CLI reaches it too", pos.Filename, pos.Line, forbidden[0], fpos)
	}

	if len(ifStmts) != 1 {
		t.Errorf("tool handler at %s:%d contains %d if statements, want exactly 1 (the json.Unmarshal error check). A handler may not hold a conditional on business state. If this branch is needed, it belongs in internal/store where the CLI reaches it too. If it is error mapping, it belongs in resultFor (internal/mcp/errors.go), which is the one permitted shared switch.", pos.Filename, pos.Line, len(ifStmts))
	} else if !isUnmarshalErrorCheck(ifStmts[0]) {
		t.Errorf("tool handler at %s:%d has exactly one if statement, but its shape is not `if err := json.Unmarshal(...); err != nil`, which is the only conditional a handler may hold", pos.Filename, pos.Line)
	}

	if storeCalls+injectedCalls != 1 {
		t.Errorf("tool handler at %s:%d makes %d data call(s): %d exported method(s) on %q (the *store.Store parameter) and %d on an injected reader (%v). Want exactly 1 in total. A handler makes one data call, its result passed straight to resultFor. If this handler needs a second source, the composition belongs in cmd/keep, injected as one function.",
			pos.Filename, pos.Line, storeCalls+injectedCalls, storeCalls, storeParamName, injectedCalls, injectedReaderNames())
	}
}

// isUnmarshalErrorCheck reports whether stmt is exactly
// `if err := json.Unmarshal(...); err != nil { ... }`, the one
// conditional a handler may hold.
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

// wantIfsOutsideHandlers is how many *ast.IfStmt may exist outside every
// tool handler, and what each one is for:
//
//  1. resultFor's `err == nil` success check.
//  2. NewServer's schema-error check.
//  3. NewServer's nil-Options.ReadyAll default.
//
// `ready_all` is served by an injected function, and a server started
// without one has to answer something rather than panic. Deciding that once
// in NewServer, where the handlers are built, keeps the handler itself
// branchless; the alternative is `if opts.ReadyAll == nil` inside the
// handler, which the handler rule forbids. The count makes each new branch
// a decision rather than drift.
const wantIfsOutsideHandlers = 3

// checkOnlyBranchOutsideHandlers enforces the package-wide half of the rule:
// exactly one *ast.SwitchStmt in the whole package (resultFor's sentinel
// classification), and exactly wantIfsOutsideHandlers *ast.IfStmt outside
// every tool handler, named by enclosing function, so a new one anywhere
// else fails by name, not just by count.
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
					// checkHandlerShape already covers handlers. Not
					// descending keeps their branches from being
					// counted again as "outside a handler".
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
		t.Fatalf("package contains %d *ast.SwitchStmt, want exactly 1 (resultFor's sentinel-classification switch, the only permitted branch): %v", len(switches), locs)
	} else if switches[0].fn != "resultFor" {
		t.Errorf("the package's one *ast.SwitchStmt is in %q at %s, want it inside resultFor (internal/mcp/errors.go)", switches[0].fn, fset.Position(switches[0].node.Pos()))
	}

	// Which functions may hold one and how many there may be in total are
	// separate assertions. The set stops a branch appearing in a third
	// function; the count stops NewServer, which holds two, from quietly
	// accumulating more.
	wantIfFuncs := map[string]bool{"NewServer": true, "resultFor": true}
	if len(ifs) != wantIfsOutsideHandlers {
		var locs []string
		for _, e := range ifs {
			locs = append(locs, fmt.Sprintf("%s (in %s)", fset.Position(e.node.Pos()), e.fn))
		}
		t.Fatalf("package contains %d *ast.IfStmt outside tool handlers, want exactly %d (resultFor's err == nil check, NewServer's schema-error check, and NewServer's nil-ReadyAll default; see wantIfsOutsideHandlers): %v", len(ifs), wantIfsOutsideHandlers, locs)
	}
	seen := map[string]bool{}
	for _, e := range ifs {
		seen[e.fn] = true
		if !wantIfFuncs[e.fn] {
			t.Errorf("unexpected *ast.IfStmt outside a tool handler in function %q at %s; want only NewServer and resultFor", e.fn, fset.Position(e.node.Pos()))
		}
	}
	for fn := range wantIfFuncs {
		if !seen[fn] {
			t.Errorf("expected an *ast.IfStmt in function %q, found none", fn)
		}
	}
}
