package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func expectSymbols(t *testing.T, got []destination, want map[string]destination) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("symbols = %+v, want %+v", got, want)
	}
	seen := make(map[string]bool)
	for _, target := range got {
		key := fmt.Sprintf("%s:%d:%d", target.Path, target.Line, target.Col)
		if seen[key] {
			t.Fatalf("duplicate symbol: %+v", target)
		}
		seen[key] = true
		expected, ok := want[target.Path]
		if !ok || target.Kind != "symbol" || target.Line != expected.Line || target.Col != expected.Col || target.Preview != expected.Preview {
			t.Fatalf("unexpected symbol %+v, want %+v", target, want)
		}
		// gopls's dynamic symbol style may qualify names by package.
		if target.Name != expected.Name && !strings.HasSuffix(target.Name, "."+expected.Name) {
			t.Fatalf("symbol name = %q, want %q (optionally package-qualified)", target.Name, expected.Name)
		}
		if target.URL != fileURI(target.Path)+fmt.Sprintf("#L%dC%d", target.Line, target.Col) {
			t.Fatalf("bad symbol URL: %+v", target)
		}
	}
}

func TestSymbolsAcrossClosedFiles(t *testing.T) {
	integrationEnvironment(t)
	root, err := canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(root, "go.mod"), "module example.com/symbols\n\ngo 1.23\n")
	anchor := filepath.Join(root, "anchor.go")
	text := "package symbols\n\n// ClosedNeedleComment is not a symbol.\n"
	putFile(t, anchor, text)
	function := filepath.Join(root, "function.go")
	typeFile := filepath.Join(root, "types.go")
	variable := filepath.Join(root, "variables.go")
	putFile(t, function, "package symbols\n\n\tfunc ClosedNeedleFunction() {}\n")
	putFile(t, typeFile, "package symbols\n\ntype ClosedNeedleType struct{}\n")
	putFile(t, variable, "package symbols\n\nvar _ = \"中😀\"; var ClosedNeedleVariable = 1\n")
	q := query{Action: "symbols", Search: "ClosedNeedle", Path: anchor, TabWidth: 4, Documents: map[string]string{anchor: text}}
	cleanupDaemon(t, q)
	expectSymbols(t, queryCLI(t, q), map[string]destination{
		function: {Line: 3, Col: 10, Name: "ClosedNeedleFunction", Preview: "func ClosedNeedleFunction() {}"},
		typeFile: {Line: 3, Col: 6, Name: "ClosedNeedleType", Preview: "type ClosedNeedleType struct{}"},
		variable: {Line: 3, Col: 21, Name: "ClosedNeedleVariable", Preview: "var _ = \"中😀\"; var ClosedNeedleVariable = 1"},
	})
	q.Search = "ClosedNeedleComment"
	expectSymbols(t, queryCLI(t, q), nil)
}

func TestSymbolsTrackUnsavedRename(t *testing.T) {
	integrationEnvironment(t)
	root, err := canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(root, "go.mod"), "module example.com/overlay\n\ngo 1.23\n")
	anchor, target := filepath.Join(root, "anchor.go"), filepath.Join(root, "target.go")
	anchorText := "package overlay\n"
	saved := "package overlay\n\nfunc OverlayBefore() {}\n"
	putFile(t, anchor, anchorText)
	putFile(t, target, saved)
	q := query{Action: "symbols", Search: "OverlayBefore", Path: anchor, TabWidth: 4, Documents: map[string]string{anchor: anchorText}}
	cleanupDaemon(t, q)
	expectSymbols(t, queryCLI(t, q), map[string]destination{
		target: {Line: 3, Col: 6, Name: "OverlayBefore", Preview: "func OverlayBefore() {}"},
	})
	q.Documents[target] = "package overlay\n\n\n\tfunc OverlayAfter() {} // unsaved\n"
	q.Search = "OverlayAfter"
	expectSymbols(t, queryCLI(t, q), map[string]destination{
		target: {Line: 4, Col: 10, Name: "OverlayAfter", Preview: "func OverlayAfter() {} // unsaved"},
	})
	q.Search = "OverlayBefore"
	expectSymbols(t, queryCLI(t, q), nil)
	// A further edit must invalidate the same persistent session's symbol index.
	q.Documents[target] = "package overlay\n\n\n\nfunc OverlayFinal() {}\n"
	q.Search = "OverlayFinal"
	expectSymbols(t, queryCLI(t, q), map[string]destination{
		target: {Line: 5, Col: 6, Name: "OverlayFinal", Preview: "func OverlayFinal() {}"},
	})
	q.Search = "OverlayAfter"
	expectSymbols(t, queryCLI(t, q), nil)
	delete(q.Documents, target)
	q.Search = "OverlayBefore"
	expectSymbols(t, queryCLI(t, q), map[string]destination{
		target: {Line: 3, Col: 6, Name: "OverlayBefore", Preview: "func OverlayBefore() {}"},
	})
	data, err := os.ReadFile(target)
	if err != nil || string(data) != saved {
		t.Fatalf("symbol overlay changed disk: %q, %v", data, err)
	}
}

func TestSymbolsRestrictWorkspaceMainModules(t *testing.T) {
	integrationEnvironment(t)
	base, err := canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot := filepath.Join(base, "workspace")
	app := filepath.Join(workspaceRoot, "app")
	external := filepath.Join(base, "external")
	dependency := filepath.Join(base, "dependency")
	nested := filepath.Join(app, "nested")
	workFile := filepath.Join(workspaceRoot, "go.work")
	putFile(t, workFile, "go 1.23\n\nuse (\n\t./app\n\t../external\n)\n")
	t.Setenv("GOWORK", workFile)
	putFile(t, filepath.Join(app, "go.mod"), "module example.com/app\n\ngo 1.23\n\nrequire (\n\texample.com/dependency v0.0.0\n\texample.com/nested v0.0.0\n)\nreplace example.com/dependency => ../../dependency\nreplace example.com/nested => ./nested\n")
	putFile(t, filepath.Join(external, "go.mod"), "module example.com/external\n\ngo 1.23\n")
	putFile(t, filepath.Join(dependency, "go.mod"), "module example.com/dependency\n\ngo 1.23\n")
	putFile(t, filepath.Join(nested, "go.mod"), "module example.com/nested\n\ngo 1.23\n")
	anchor := filepath.Join(app, "anchor.go")
	anchorText := "package app\n\nimport (\n\t_ \"example.com/dependency\"\n\t_ \"example.com/nested\"\n)\n\nfunc ScopeNeedleApp() {}\n"
	putFile(t, anchor, anchorText)
	member := filepath.Join(external, "member.go")
	putFile(t, member, "package external\n\nfunc ScopeNeedleMember() {}\n")
	putFile(t, filepath.Join(dependency, "dependency.go"), "package dependency\n\nfunc ScopeNeedleDependency() {}\n")
	putFile(t, filepath.Join(nested, "nested.go"), "package nested\n\nfunc ScopeNeedleNested() {}\n")
	// gopls may index the Go file via its workspace-facing name, but its real
	// source is outside every enrolled main module and must never be exposed.
	escape := filepath.Join(base, "escape.go")
	putFile(t, escape, "package app\n\nfunc ScopeNeedleEscape() {}\n")
	if err := os.Symlink(escape, filepath.Join(app, "escaped.go")); err != nil {
		t.Fatal(err)
	}
	q := query{Action: "symbols", Search: "ScopeNeedle", Path: anchor, TabWidth: 4, Documents: map[string]string{anchor: anchorText}}
	cleanupDaemon(t, q)
	expectSymbols(t, queryCLI(t, q), map[string]destination{
		anchor: {Line: 8, Col: 6, Name: "ScopeNeedleApp", Preview: "func ScopeNeedleApp() {}"},
		member: {Line: 3, Col: 6, Name: "ScopeNeedleMember", Preview: "func ScopeNeedleMember() {}"},
	})
	for _, search := range []string{"ScopeNeedleDependency", "ScopeNeedleNested", "ScopeNeedleEscape"} {
		q.Search = search
		expectSymbols(t, queryCLI(t, q), nil)
	}
}

func TestProjectSymbolDirectoryBoundaries(t *testing.T) {
	base, err := canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "project")
	nested := filepath.Join(root, "nested")
	member := filepath.Join(base, "member")
	for _, dir := range []string{root, nested, member} {
		putFile(t, filepath.Join(dir, "go.mod"), "module example.com/test\n")
	}
	for _, tc := range []struct {
		name  string
		dir   string
		roots []string
		want  bool
	}{
		{"main module", root, []string{root}, true},
		{"subpackage", filepath.Join(root, "pkg"), []string{root}, true},
		{"sibling prefix", root + "-other", []string{root}, false},
		{"unregistered nested module", nested, []string{root}, false},
		{"nested subpackage", filepath.Join(nested, "pkg"), []string{root}, false},
		{"enrolled nested member", nested, []string{root, nested}, true},
		{"external member", member, []string{root, member}, true},
		{"external dependency", member, []string{root}, false},
		{"vendor", filepath.Join(root, "vendor", "example.com", "dep"), []string{root}, false},
		{"no main modules", root, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := projectSymbolDirectory(tc.dir, tc.roots)
			if err != nil || got != tc.want {
				t.Fatalf("projectSymbolDirectory(%q, %q) = %v, %v; want %v", tc.dir, tc.roots, got, err, tc.want)
			}
		})
	}
	outside := filepath.Join(member, "escape.go")
	putFile(t, outside, "package test\n")
	link := filepath.Join(root, "escape.go")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := canonical(link)
	if err != nil {
		t.Fatal(err)
	}
	if included, err := projectSymbolDirectory(filepath.Dir(resolved), []string{root}); err != nil || included {
		t.Fatalf("symlink escape included: %v, %v", included, err)
	}
}

func TestSymbolsRejectEmptySearch(t *testing.T) {
	for _, search := range []string{"", " \t\r\n"} {
		// No RPC is installed: validation must happen before contacting gopls.
		_, err := (&session{}).symbols(context.Background(), query{Search: search})
		if err == nil || !strings.Contains(err.Error(), "symbol search must not be empty") {
			t.Fatalf("search %q: got %v, want empty symbol search error", search, err)
		}
	}
}
