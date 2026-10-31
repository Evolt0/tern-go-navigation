package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareQueryUsesSelectionByteOffsets(t *testing.T) {
	root := watcherTestRoot(t)
	path := filepath.Join(root, "selected.go")
	text := "package selected\r\n\r\n// 中😀 é\r\nfunc 目标函数() {}\r\nfunc Caller() {\r\n\t/* 中😀é */ 目标函数()\r\n}\r\n"
	putFile(t, path, text)
	start := strings.LastIndex(text, "目标函数")
	end := start + len("目标函数")
	for _, tc := range []struct {
		name      string
		selection editorSelection
		col       int
		want      position
	}{
		{"forward", editorSelection{start, end}, 25, position{5, 13}},
		{"reverse", editorSelection{end, start}, 17, position{5, 13}},
		{"collapsed", editorSelection{end, end}, 25, position{5, 17}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := query{Path: path, Line: 6, Col: tc.col, TabWidth: 4, Selection: &tc.selection, Documents: map[string]string{path: text}}
			_, got, err := prepareQuery(q)
			if err != nil || got != tc.want {
				t.Fatalf("position = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
	for _, selection := range []editorSelection{
		{-1, end}, {start, len(text) + 1}, {start + 1, end}, {end - 1, start},
	} {
		q := query{Path: path, Line: 6, Col: 25, TabWidth: 4, Selection: &selection, Documents: map[string]string{path: text}}
		if _, _, err := prepareQuery(q); err == nil {
			t.Errorf("accepted invalid UTF-8 selection %+v", selection)
		}
	}
}

func TestSelectedSymbolNavigation(t *testing.T) {
	integrationEnvironment(t)
	root := watcherTestRoot(t)
	putFile(t, filepath.Join(root, "go.mod"), "module example.com/selected\n\ngo 1.23\n")
	target, caller := filepath.Join(root, "target.go"), filepath.Join(root, "caller.go")
	name := "templateDefinitionLanguage"
	targetText := "package selected\n\n// 中😀 é\nfunc " + name + "() {}\n"
	callerText := "package selected\n\nfunc Caller() {\n\t" + name + "()\n}\n"
	putFile(t, target, targetText)
	putFile(t, caller, callerText)
	documents := map[string]string{target: targetText, caller: callerText}
	start := strings.Index(targetText, name)
	q := query{
		Path: target, Line: 4, Col: 6 + len(name), TabWidth: 4,
		Selection: &editorSelection{Anchor: start, Head: start + len(name)}, Documents: documents,
	}
	cleanupDaemon(t, q)
	want := map[string]destination{
		target: {Line: 4, Col: 6, Kind: "definition"},
		caller: {Line: 4, Col: 5, Kind: "reference"},
	}
	// Double-click selection ends at '(' rather than inside the function name.
	expectDestinations(t, queryCLI(t, q), want)

	// Reverse selection still resolves the same symbol, this time at a call.
	start = strings.Index(callerText, name)
	q.Path, q.Line, q.Col = caller, 4, 5
	q.Selection = &editorSelection{Anchor: start + len(name), Head: start}
	expectDestinations(t, queryCLI(t, q), want)

	// Offsets belong to the editor overlay, not the saved source file.
	unsavedName := "UnsavedSelectedTarget"
	documents[target] = strings.Replace(targetText, "func "+name, "\nfunc "+unsavedName, 1)
	documents[caller] = strings.Replace(callerText, name, unsavedName, 1)
	start = strings.Index(documents[caller], unsavedName)
	q.Col = 5 + len(unsavedName)
	q.Selection = &editorSelection{Anchor: start, Head: start + len(unsavedName)}
	want[target] = destination{Line: 5, Col: 6, Kind: "definition"}
	expectDestinations(t, queryCLI(t, q), want)
	for path, saved := range map[string]string{target: targetText, caller: callerText} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != saved {
			t.Fatalf("navigation changed saved source %s: %q, %v", path, got, err)
		}
	}
}
