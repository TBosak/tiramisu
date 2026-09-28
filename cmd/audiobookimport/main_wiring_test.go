package main

// Requirement coverage (A8, review-2.md): "the standalone command's main()
// actually installs the returned live dependencies instead of continuing to
// call RunCLI with Dependencies{}." live_dependencies_test.go already proves
// LiveDependencies itself works; this file proves main() actually calls it
// and passes the result to RunCLI, via AST inspection of main.go's own
// func main body - not a text search, so a comment mentioning
// "LiveDependencies" cannot satisfy this, and the review's counterexample
// (main() still calling RunCLI with a bare Dependencies{}) is exactly what
// this test is designed to catch.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestMainGo_Main_InstallsLiveDependenciesIntoRunCLI(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var mainFn *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name != nil && fn.Name.Name == "main" {
			mainFn = fn
		}
	}
	if mainFn == nil || mainFn.Body == nil {
		t.Fatal("main.go has no func main() body to inspect")
	}

	// liveDepsVars collects every identifier assigned from a call to
	// LiveDependencies(...), handling both the two-value ("deps, err :=
	// LiveDependencies(cfg)") and unlikely one-value forms.
	liveDepsVars := map[string]bool{}
	var runCLICall *ast.CallExpr

	ast.Inspect(mainFn.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			if len(node.Rhs) == 1 {
				if call, ok := node.Rhs[0].(*ast.CallExpr); ok {
					if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "LiveDependencies" {
						for _, lhs := range node.Lhs {
							if lhsIdent, ok := lhs.(*ast.Ident); ok {
								liveDepsVars[lhsIdent.Name] = true
							}
						}
					}
				}
			}
		case *ast.CallExpr:
			if ident, ok := node.Fun.(*ast.Ident); ok && ident.Name == "RunCLI" {
				runCLICall = node
			}
		}
		return true
	})

	if len(liveDepsVars) == 0 {
		t.Fatal("main() never calls LiveDependencies(...); the standalone command cannot construct a live dependency graph")
	}
	if runCLICall == nil {
		t.Fatal("main() never calls RunCLI(...)")
	}
	if len(runCLICall.Args) < 3 {
		t.Fatalf("RunCLI(...) call has %d argument(s), want at least 3 (ctx, cfg, deps, ...)", len(runCLICall.Args))
	}

	depsArg := runCLICall.Args[2]
	if depsIdent, ok := depsArg.(*ast.Ident); ok && liveDepsVars[depsIdent.Name] {
		return // deps argument is a variable that was assigned from LiveDependencies(...)
	}
	if _, ok := depsArg.(*ast.CompositeLit); ok {
		t.Fatalf("RunCLI's third argument is a bare composite literal (e.g. Dependencies{}), not the result of LiveDependencies(...)")
	}
	t.Fatalf("RunCLI's third argument is not a variable assigned from LiveDependencies(...): %#v", depsArg)
}
