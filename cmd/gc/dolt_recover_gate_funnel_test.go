package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The city lead's requirement for ga-amol9 was ONE shared code path to
// the managed-Dolt recover op, for every caller — controller tick,
// health order, prime, bd wrapper, `gc dolt health`, `gc beads health`,
// and any future one — so that neither the cooldown nor the liveness
// check can be skipped by taking a different route to the same effect.
// Five routes reaching one effect with no single gate was the shape of
// the outage.
//
// Go cannot enforce that at compile time inside one package, so these
// tests assert it structurally, on the AST rather than on names. They
// are written to survive the defeat that beat #204's tripwire — holding
// the op in a variable — by requiring every provider-op call site to
// pass a literal, and then requiring that no literal is the recover op.
//
// KNOWN GAP, stated rather than left implied: an op string assembled at
// run time (read from config, or built across statements) is invisible
// to an AST check. Closing that needs the provider-op runner moved
// behind a package boundary where the recover op is unexported.

const recoverGateFile = "dolt_recover_gate.go"

// parseNonTestCmdGC parses every non-test Go file in cmd/gc, keyed by
// base name. It walks the directory itself rather than using
// parser.ParseDir, which is deprecated as of Go 1.25.
func parseNonTestCmdGC(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read cmd/gc: %v", err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		if parsed.Name.Name != "main" {
			continue
		}
		files[name] = parsed
	}
	if len(files) == 0 {
		t.Fatal("cmd/gc parsed to zero non-test files")
	}
	return fset, files
}

// providerOpRunners are the functions that hand an operation to the
// provider script, and the argument index at which the operation sits.
//
//	runProviderOp(script, cityPath, args...)
//	runProviderOpWithEnv(script, environ, args...)
//	runProviderOpWithEnvContext(ctx, script, environ, args...)
var providerOpRunners = map[string]int{
	"runProviderOp":               2,
	"runProviderOpWithEnv":        2,
	"runProviderOpWithEnvContext": 3,
}

// providerOpForwarders are the functions allowed to pass an operation
// through without naming it — they spread their own variadic parameter.
// A forwarder cannot enforce the recover guard, so the set is pinned
// here and TestProviderOpForwardersAreNotARouteToRecover asserts nothing
// reaches the recover op through one.
//
//	runProviderOp          → runProviderOpWithEnv (no production callers)
//	runProviderOpWithEnv   → runProviderOpWithEnvContext (start/stop/health/init)
//
// Neither is reachable with the recover op today, and the second half of
// TestProviderOpForwardersAreNotARouteToRecover is what keeps that true.
var providerOpForwarders = map[string]bool{
	"runProviderOp":        true,
	"runProviderOpWithEnv": true,
}

// providerOpArg returns the operation argument of a provider-op call,
// and whether the call is one.
func providerOpArg(call *ast.CallExpr) (ast.Expr, bool) {
	ident, ok := call.Fun.(*ast.Ident)
	if !ok {
		return nil, false
	}
	idx, ok := providerOpRunners[ident.Name]
	if !ok || len(call.Args) <= idx {
		return nil, false
	}
	return call.Args[idx], true
}

// enclosingFuncName returns the name of the FuncDecl containing pos, or
// "" when pos is inside a func literal assigned to a package-level var.
func enclosingFuncName(file *ast.File, pos token.Pos) string {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if fn.Pos() <= pos && pos <= fn.End() {
			return fn.Name.Name
		}
	}
	return ""
}

// Every provider-op call site must name its operation with a literal or
// the recover constant. A call that passes a variable could smuggle the
// recover op past the guard without the op string ever appearing at the
// call site — the exact substitution that defeated #204's census.
func TestProviderOpCallSitesNameTheirOperationStatically(t *testing.T) {
	fset, files := parseNonTestCmdGC(t)

	for name, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			arg, isProviderOp := providerOpArg(call)
			if !isProviderOp {
				return true
			}
			pos := fset.Position(arg.Pos())
			switch a := arg.(type) {
			case *ast.BasicLit:
				if a.Kind != token.STRING {
					t.Errorf("%s: provider op argument is a non-string literal", pos)
					return true
				}
				op, err := strconv.Unquote(a.Value)
				if err != nil {
					t.Errorf("%s: unparsable provider op literal %s", pos, a.Value)
					return true
				}
				if op == managedDoltRecoverOpName {
					t.Errorf("%s: passes the recover op as a bare literal; it must go through runGuardedManagedDoltRecover, which applies the cooldown and the liveness check (ga-amol9)", pos)
				}
			case *ast.Ident:
				if a.Name == "managedDoltRecoverOpName" {
					return true
				}
				// A variadic forwarder spreading its own parameter is the
				// one legitimate non-literal shape, and the set of them is
				// pinned below.
				if call.Ellipsis != token.NoPos && providerOpForwarders[enclosingFuncName(file, call.Pos())] {
					return true
				}
				t.Errorf("%s (%s): provider op is the variable %q, so the operation cannot be checked statically; name it with a literal", pos, name, a.Name)
			default:
				t.Errorf("%s (%s): provider op argument is neither a literal nor a known constant", pos, name)
			}
			return true
		})
	}
}

// A forwarder passes its caller's operation through verbatim, so it
// cannot enforce the recover guard. Two things must therefore hold: the
// set of forwarders does not grow silently, and no production code
// reaches the recover op through one.
func TestProviderOpForwardersAreNotARouteToRecover(t *testing.T) {
	fset, files := parseNonTestCmdGC(t)

	found := map[string]bool{}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || call.Ellipsis == token.NoPos {
				return true
			}
			if _, isProviderOp := providerOpArg(call); !isProviderOp {
				return true
			}
			if name := enclosingFuncName(file, call.Pos()); name != "" {
				found[name] = true
			}
			return true
		})
	}
	for name := range found {
		if !providerOpForwarders[name] {
			t.Errorf("%s forwards an unnamed provider op but is not in providerOpForwarders; if it can reach the recover op it bypasses the guard (ga-amol9)", name)
		}
	}

	// Nothing may call a forwarder with the recover op.
	for fileName, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || !providerOpForwarders[id.Name] {
				return true
			}
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if op, err := strconv.Unquote(lit.Value); err == nil && op == managedDoltRecoverOpName {
					t.Errorf("%s (%s): reaches the recover op through the forwarder %s, bypassing runGuardedManagedDoltRecover (ga-amol9)", fset.Position(lit.Pos()), fileName, id.Name)
				}
			}
			return true
		})
	}
}

// The recover op name is referenced exactly once outside its own
// declaration, in the guard file.
func TestManagedDoltRecoverOpNameHasASingleUse(t *testing.T) {
	fset, files := parseNonTestCmdGC(t)

	uses := []string{}
	for name, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			// Skip the const declaration itself.
			if spec, ok := n.(*ast.ValueSpec); ok {
				for _, id := range spec.Names {
					if id.Name == "managedDoltRecoverOpName" {
						return false
					}
				}
			}
			id, ok := n.(*ast.Ident)
			if !ok || id.Name != "managedDoltRecoverOpName" {
				return true
			}
			uses = append(uses, fset.Position(id.Pos()).String())
			if name != recoverGateFile {
				t.Errorf("%s: the recover op name is used outside %s", fset.Position(id.Pos()), recoverGateFile)
			}
			return true
		})
	}
	if len(uses) != 1 {
		t.Fatalf("recover op name used %d times outside its declaration (%v), want exactly 1 — the guarded runner", len(uses), uses)
	}
}

// The provider runner that actually performs the recover is called from
// exactly one function, and that function is the guard. This is the
// funnel: the runner is a var so tests can fake the provider script, and
// this test is what stops that seam from becoming a second route.
func TestManagedDoltRecoverRunnerIsCalledOnlyByTheGuard(t *testing.T) {
	fset, files := parseNonTestCmdGC(t)

	callers := map[string]string{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "managedDoltRecoverRunner" {
					callers[fn.Name.Name] = fset.Position(call.Pos()).String()
				}
				return true
			})
		}
	}

	if len(callers) != 1 {
		t.Fatalf("managedDoltRecoverRunner is called from %d functions (%v), want exactly 1", len(callers), callers)
	}
	if _, ok := callers["runGuardedManagedDoltRecover"]; !ok {
		t.Fatalf("managedDoltRecoverRunner is called from %v, want only runGuardedManagedDoltRecover", callers)
	}
}

// Both historical routes to the recover op now go through the guard.
// This pins the two call sites by name so that deleting the guard call
// from either one fails here as well as in the behavioral tests.
func TestBothRecoverRoutesCallTheGuard(t *testing.T) {
	_, files := parseNonTestCmdGC(t)

	want := map[string]bool{
		"recoverManagedBDCommand":    false, // the bd runner's route
		"healthBeadsProviderContext": false, // the health patrol's route
	}

	for _, file := range files {
		// recoverManagedBDCommand is a package-level var holding a func
		// literal, so it is not a FuncDecl; check both shapes.
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.ValueSpec:
				for i, id := range node.Names {
					if _, tracked := want[id.Name]; !tracked || i >= len(node.Values) {
						continue
					}
					if callsGuard(node.Values[i]) {
						want[id.Name] = true
					}
				}
			case *ast.FuncDecl:
				if _, tracked := want[node.Name.Name]; tracked && node.Body != nil && callsGuard(node.Body) {
					want[node.Name.Name] = true
				}
			}
			return true
		})
	}

	for name, found := range want {
		if !found {
			t.Errorf("%s does not call runGuardedManagedDoltRecover: every route to the recover op must pass the one gate (ga-amol9)", name)
		}
	}
}

func callsGuard(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "runGuardedManagedDoltRecover" {
			found = true
			return false
		}
		return true
	})
	return found
}
