package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "--daemon" {
		main()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "--query" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
		main()
		return
	}
	os.Exit(m.Run())
}

func TestHelperLifetimeFollowsEditor(t *testing.T) {
	if helperExpired(os.Getpid(), 2*idleTimeout) {
		t.Fatal("discarded a live editor's index after an idle interval")
	}
	owner := exec.Command("/usr/bin/true")
	if err := owner.Run(); err != nil {
		t.Fatal(err)
	}
	if !helperExpired(owner.Process.Pid, 0) {
		t.Fatal("retained an index after its editor exited")
	}
	if !helperExpired(0, 2*idleTimeout) {
		t.Fatal("standalone CLI session lost its idle cleanup")
	}
}

func TestUnicodeCoordinates(t *testing.T) {
	text := "package p\r\n\t/* 中😀é */ target()\r\n"
	pos, err := lspPosition(text, 2, 17, 4)
	if err != nil {
		t.Fatal(err)
	}
	if pos != (position{Line: 1, Character: 13}) {
		t.Fatalf("got %+v, want line 1, UTF-16 column 13", pos)
	}
	col, err := ternColumn(text, pos, 4)
	if err != nil || col != 17 {
		t.Fatalf("reverse conversion: col=%d err=%v", col, err)
	}
	if _, err := ternColumn(text, position{Line: 1, Character: 6}, 4); err == nil {
		t.Fatal("accepted a position inside an emoji surrogate pair")
	}
	if _, err := lspPosition(text, 2, 0, 4); err == nil {
		t.Fatal("accepted column zero")
	}
	if _, err := lspPosition(text, 20, 1, 4); err == nil {
		t.Fatal("accepted a line outside the document")
	}
}

func TestDestinationPreviewsUseLiveSourceLines(t *testing.T) {
	file, err := canonical(filepath.Join(t.TempDir(), "calls.go"))
	if err != nil {
		t.Fatal(err)
	}
	disk := "package p\r\n\tTarget(\"磁盘\")\r\n\tTarget(\"第二处\")\r\n"
	putFile(t, file, disk)
	locations := []location{
		{URI: fileURI(file), Range: sourceRange{Start: position{Line: 1, Character: 1}}},
		{URI: fileURI(file), Range: sourceRange{Start: position{Line: 2, Character: 1}}},
	}
	for _, tc := range []struct {
		name      string
		documents map[string]string
		want      [2]string
	}{
		{"disk", map[string]string{}, [2]string{`Target("磁盘")`, `Target("第二处")`}},
		{"unsaved", map[string]string{file: "package p\r\n\tTarget(\"未保存 😀\")  // keep  spaces\r\n\tTarget(\"缓冲区第二处\")\r\n"},
			[2]string{`Target("未保存 😀")  // keep  spaces`, `Target("缓冲区第二处")`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := destinations(locations, "reference", query{TabWidth: 4, Documents: tc.documents}, make(map[string]bool))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("destinations = %+v, want previews %q", got, tc.want)
			}
			for i, target := range got {
				if target.Preview != tc.want[i] || target.Line != i+2 || target.Col != 5 {
					t.Fatalf("destination = %+v, want line %d col 5 preview %q", target, i+2, tc.want[i])
				}
			}
		})
	}
}

func TestWorkspacePrefersEnclosingGoWork(t *testing.T) {
	root := t.TempDir()
	module := filepath.Join(root, "module")
	if err := os.Mkdir(module, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "go.mod"), []byte("module example.com/p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(module, "main.go")
	if got := workspace(file); got != module {
		t.Fatalf("without go.work: got %s, want %s", got, module)
	}
	if err := os.WriteFile(filepath.Join(root, "go.work"), []byte("use ./module\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := workspace(file); got != root {
		t.Fatalf("with go.work: got %s, want %s", got, root)
	}
}

func integrationEnvironment(t *testing.T) {
	t.Helper()
	gopls := os.Getenv("GOPLS")
	if gopls == "" {
		var err error
		gopls, err = exec.LookPath("gopls")
		if err != nil {
			gopls = filepath.Join(os.Getenv("HOME"), "go", "bin", "gopls")
			if _, err := os.Stat(gopls); err != nil {
				t.Skip("integration regression requires gopls")
			}
		}
	}
	t.Setenv("GOPLS", gopls)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GOROOT", runtime.GOROOT())
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	t.Setenv("GOPROXY", "off")
	t.Setenv("TERN_WINDOW_KEY", t.Name())
}

func putFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func queryCLI(t *testing.T, q query) []destination {
	t.Helper()
	data, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout+5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "--query")
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("query: %v\n%s", err, stderr.String())
	}
	var result []destination
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode %q: %v", output, err)
	}
	return result
}

func cleanupDaemon(t *testing.T, q query) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := daemonQuery(ctx, q, true); err != nil {
			t.Errorf("shutdown daemon: %v", err)
		}
	})
}

func expectDestinations(t *testing.T, got []destination, want map[string]destination) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("destinations = %+v, want %+v", got, want)
	}
	references := false
	seen := make(map[string]bool)
	for _, target := range got {
		if target.Kind == "reference" {
			references = true
		} else if target.Kind != "definition" || references {
			t.Fatalf("not definitions-first: %+v", got)
		}
		key := fmt.Sprintf("%s:%d:%d", target.Path, target.Line, target.Col)
		if seen[key] {
			t.Fatalf("duplicate destination %+v", target)
		}
		seen[key] = true
		expected, ok := want[target.Path]
		if !ok || target.Line != expected.Line || target.Col != expected.Col || target.Kind != expected.Kind {
			t.Fatalf("unexpected destination %+v, want %+v", target, want)
		}
		if !strings.HasSuffix(target.URL, fmt.Sprintf("#L%dC%d", target.Line, target.Col)) {
			t.Fatalf("bad URL: %+v", target)
		}
	}
}

func TestPersistentNavigationWithoutGoOnPath(t *testing.T) {
	integrationEnvironment(t)
	root, err := canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target, caller, other := filepath.Join(root, "target.go"), filepath.Join(root, "caller.go"), filepath.Join(root, "other.go")
	putFile(t, filepath.Join(root, "go.mod"), "module example.com/navigation\n\ngo 1.23\n")
	putFile(t, target, "package navigation\n\nfunc Saved() {}\n")
	putFile(t, caller, "package navigation\n\nfunc Caller() { Saved() }\n")
	putFile(t, other, "package navigation\n\nfunc Other() { Saved() }\n")
	documents := map[string]string{
		target: "package navigation\n\n\nfunc Unsaved() {}\n",
		caller: "package navigation\n\nfunc Caller() { Unsaved() }\n",
		other:  "package navigation\n\nfunc Other() { Unsaved() }\n",
	}
	q := query{Path: caller, Line: 3, Col: 17, TabWidth: 4, Documents: documents}
	cleanupDaemon(t, q)
	want := map[string]destination{
		target: {Line: 4, Col: 6, Kind: "definition"},
		caller: {Line: 3, Col: 17, Kind: "reference"},
		other:  {Line: 3, Col: 16, Kind: "reference"},
	}
	expectDestinations(t, queryCLI(t, q), want)
	// A second short-lived CLI must reuse the same overlays and workspace.
	expectDestinations(t, queryCLI(t, q), want)
	// Definitions win when querying the declaration itself as well.
	q.Path, q.Line, q.Col = target, 4, 6
	expectDestinations(t, queryCLI(t, q), want)
	q.Path, q.Line, q.Col = caller, 3, 17
	documents[target] = "package navigation\n\n\n\nfunc Unsaved() {}\n"
	want[target] = destination{Line: 5, Col: 6, Kind: "definition"}
	expectDestinations(t, queryCLI(t, q), want)
	// Closing target and other discards those overlays, restoring disk content.
	delete(documents, target)
	delete(documents, other)
	documents[caller] = "package navigation\n\nfunc Caller() { Saved() }\n"
	want[target] = destination{Line: 3, Col: 6, Kind: "definition"}
	expectDestinations(t, queryCLI(t, q), want)
	// Save a closed file externally, then query immediately: no test sleeps.
	putFile(t, target, "package navigation\n\n\n\n\nfunc Saved() {}\n")
	want[target] = destination{Line: 6, Col: 6, Kind: "definition"}
	expectDestinations(t, queryCLI(t, q), want)
	added := filepath.Join(root, "added.go")
	putFile(t, added, "package navigation\n\nfunc Added() { Saved() }\n")
	want[added] = destination{Line: 3, Col: 16, Kind: "reference"}
	expectDestinations(t, queryCLI(t, q), want)
	if err := os.Remove(added); err != nil {
		t.Fatal(err)
	}
	delete(want, added)
	expectDestinations(t, queryCLI(t, q), want)
	// Reopen a closed overlay without changing its saved backing file.
	documents[target] = "package navigation\n\nfunc Saved() {}\n"
	want[target] = destination{Line: 3, Col: 6, Kind: "definition"}
	expectDestinations(t, queryCLI(t, q), want)
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "package navigation\n\n\n\n\nfunc Saved() {}\n" {
		t.Fatalf("overlay modified disk: %q, %v", data, err)
	}
}

func TestPersistentExternalReplacementAndModuleEdits(t *testing.T) {
	integrationEnvironment(t)
	root, err := canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, dep, second := filepath.Join(root, "app"), filepath.Join(root, "dep"), filepath.Join(root, "second")
	module := "module example.com/app\n\ngo 1.23\n\nrequire example.com/dep v0.0.0\nreplace example.com/dep => ../dep\n"
	putFile(t, filepath.Join(app, "go.mod"), module)
	putFile(t, filepath.Join(dep, "go.mod"), "module example.com/dep\n\ngo 1.23\n")
	putFile(t, filepath.Join(second, "go.mod"), "module example.com/dep\n\ngo 1.23\n")
	definition := filepath.Join(dep, "dep.go")
	putFile(t, definition, "package dep\n\nfunc Target() {}\n")
	putFile(t, filepath.Join(second, "dep.go"), "package dep\n\n\nfunc Target() {}\n")
	caller := filepath.Join(app, "main.go")
	text := "package app\n\nimport \"example.com/dep\"\n\nfunc Caller() { dep.Target() }\n"
	putFile(t, caller, text)
	q := query{Path: caller, Line: 5, Col: 21, TabWidth: 4, Documents: map[string]string{caller: text}}
	cleanupDaemon(t, q)
	want := map[string]destination{definition: {Line: 3, Col: 6, Kind: "definition"}, caller: {Line: 5, Col: 21, Kind: "reference"}}
	expectDestinations(t, queryCLI(t, q), want)
	putFile(t, definition, "package dep\n\n\n\nfunc Target() {}\n")
	want[definition] = destination{Line: 5, Col: 6, Kind: "definition"}
	expectDestinations(t, queryCLI(t, q), want)
	putFile(t, filepath.Join(app, "go.mod"), strings.Replace(module, "=> ../dep", "=> ../second", 1))
	delete(want, definition)
	want[filepath.Join(second, "dep.go")] = destination{Line: 4, Col: 6, Kind: "definition"}
	expectDestinations(t, queryCLI(t, q), want)
}

func TestConcurrentDaemonStartup(t *testing.T) {
	integrationEnvironment(t)
	root, err := canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(root, "go.mod"), "module example.com/concurrent\n\ngo 1.23\n")
	file := filepath.Join(root, "main.go")
	text := "package concurrent\n\nfunc Target() {}\nfunc Caller() { Target() }\n"
	putFile(t, file, text)
	q := query{Path: file, Line: 4, Col: 17, TabWidth: 4, Documents: map[string]string{file: text}}
	cleanupDaemon(t, q)
	var workers sync.WaitGroup
	errors := make(chan error, 4)
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
			defer cancel()
			got, err := daemonQuery(ctx, q, false)
			if err == nil && (len(got) != 2 || got[0].Kind != "definition" || got[1].Kind != "reference") {
				err = fmt.Errorf("unexpected combined result: %+v", got)
			}
			errors <- err
		}()
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestPersistentExternalWorkspaceModules(t *testing.T) {
	integrationEnvironment(t)
	root, err := canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot := filepath.Join(root, "workspace")
	app := filepath.Join(workspaceRoot, "app")
	dep, second := filepath.Join(root, "dep"), filepath.Join(root, "second")
	workFile := filepath.Join(workspaceRoot, "go.work")
	putFile(t, workFile, "go 1.23\n\nuse (\n ./app\n ../dep\n)\n")
	t.Setenv("GOWORK", workFile)
	putFile(t, filepath.Join(app, "go.mod"), "module example.com/app\n\ngo 1.23\n\nrequire example.com/dep v0.0.0\n")
	putFile(t, filepath.Join(dep, "go.mod"), "module example.com/dep\n\ngo 1.23\n")
	putFile(t, filepath.Join(second, "go.mod"), "module example.com/dep\n\ngo 1.23\n")
	definition := filepath.Join(dep, "dep.go")
	putFile(t, definition, "package dep\n\nfunc Target() {}\n")
	putFile(t, filepath.Join(second, "dep.go"), "package dep\n\n\nfunc Target() {}\n")
	caller := filepath.Join(app, "main.go")
	text := "package app\n\nimport \"example.com/dep\"\n\nfunc Caller() { dep.Target() }\n"
	putFile(t, caller, text)
	q := query{Path: caller, Line: 5, Col: 21, TabWidth: 4, Documents: map[string]string{caller: text}}
	cleanupDaemon(t, q)
	want := map[string]destination{definition: {Line: 3, Col: 6, Kind: "definition"}, caller: {Line: 5, Col: 21, Kind: "reference"}}
	expectDestinations(t, queryCLI(t, q), want)
	putFile(t, definition, "package dep\n\n\n\nfunc Target() {}\n")
	want[definition] = destination{Line: 5, Col: 6, Kind: "definition"}
	expectDestinations(t, queryCLI(t, q), want)
	putFile(t, workFile, "go 1.23\n\nuse (\n ./app\n ../second\n)\n")
	delete(want, definition)
	want[filepath.Join(second, "dep.go")] = destination{Line: 4, Col: 6, Kind: "definition"}
	expectDestinations(t, queryCLI(t, q), want)
}

func TestWatcherImmediateAtomicReplacementAndDirectoryDeletion(t *testing.T) {
	root, err := canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "nested", "file.go")
	putFile(t, file, "package nested\n")
	w, err := newFileWatcher(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Editors commonly save by renaming a temporary file over the original.
	temporary := filepath.Join(root, "replacement")
	putFile(t, temporary, "package nested\n\nvar Changed = true\n")
	if err := os.Rename(temporary, file); err != nil {
		t.Fatal(err)
	}
	changes, _, err := w.changes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, change := range changes {
		if change.URI == fileURI(file) && change.Type != 3 {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing atomic replacement event: %+v", changes)
	}
	if err := os.RemoveAll(filepath.Dir(file)); err != nil {
		t.Fatal(err)
	}
	changes, _, err = w.changes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, change := range changes {
		if change.URI == fileURI(file) && change.Type == 3 {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing directory deletion event: %+v", changes)
	}
}

func TestSharedGoplsKeepsClientBuffersAndLifetimesIndependent(t *testing.T) {
	integrationEnvironment(t)
	runtimeDir, err := os.MkdirTemp("/tmp", "tern-gopls-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	gopls := os.Getenv("GOPLS")
	root, err := canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(root, "go.mod"), "module example.com/shared\n\ngo 1.23\n")
	target, caller := filepath.Join(root, "target.go"), filepath.Join(root, "caller.go")
	putFile(t, target, "package shared\n\nfunc Saved() {}\n")
	text := "package shared\n\nfunc Caller() { SharedTarget() }\n"
	putFile(t, caller, text)
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	socket := filepath.Join(runtimeDir, "gopls.sock")
	daemon := exec.CommandContext(ctx, gopls, "serve", "-listen=unix;"+socket, "-listen.timeout=5s")
	daemon.Dir = root
	daemon.Env, err = goEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	daemon.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	daemon.WaitDelay = time.Second
	var daemonLog boundedLog
	daemon.Stdout, daemon.Stderr = &daemonLog, &daemonLog
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = daemon.Wait(); close(done) }()
	t.Cleanup(func() {
		// Reap only this fixture's process group, including telemetry, even if
		// an assertion fails before the natural idle shutdown can be observed.
		_ = syscall.Kill(-daemon.Process.Pid, syscall.SIGKILL)
		<-done
	})
	t.Setenv("TERN_TEST_GOPLS", gopls)
	t.Setenv("TERN_TEST_GOPLS_SOCKET", socket)
	wrapper := filepath.Join(t.TempDir(), "gopls")
	putFile(t, wrapper, "#!/bin/sh\nif [ \"$1\" = -remote=auto ]; then\n shift\n exec \"$TERN_TEST_GOPLS\" \"-remote=unix;$TERN_TEST_GOPLS_SOCKET\" \"$@\"\nfi\nexec \"$TERN_TEST_GOPLS\" \"$@\"\n")
	if err := os.Chmod(wrapper, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOPLS", wrapper)
	first, err := newSession(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	second, err := newSession(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	type serverState struct {
		Clients []struct {
			SessionID string `json:"sessionID"`
		} `json:"clients"`
	}
	// The public inspector connects directly to the daemon. Its own connection
	// is the third client, alongside the two independently initialized editors.
	inspect := exec.CommandContext(ctx, gopls, "-remote=unix;"+socket, "remote", "sessions")
	inspect.Dir, inspect.Env = root, first.env
	output, err := inspect.Output()
	if err != nil {
		t.Fatalf("inspect shared gopls: %v", err)
	}
	var shared serverState
	if err := json.Unmarshal(output, &shared); err != nil {
		t.Fatal(err)
	}
	if len(shared.Clients) != 3 {
		t.Fatalf("expected two editors and the inspector on one daemon: %+v", shared)
	}
	q := query{Path: caller, Line: 3, Col: 17, TabWidth: 4}
	pos := position{Line: 2, Character: 16}
	check := func(s *session, line int) {
		t.Helper()
		q.Documents = map[string]string{
			target: "package shared\n" + strings.Repeat("\n", line-2) + "func SharedTarget() {}\n",
			caller: text,
		}
		got, err := s.navigate(ctx, q, pos)
		if err != nil {
			t.Fatal(err)
		}
		expectDestinations(t, got, map[string]destination{
			target: {Line: line, Col: 6, Kind: "definition"},
			caller: {Line: 3, Col: 17, Kind: "reference"},
		})
	}
	check(first, 3)
	check(second, 5)
	check(first, 3)
	second.close()
	check(first, 3)
	if data, err := os.ReadFile(target); err != nil || string(data) != "package shared\n\nfunc Saved() {}\n" {
		t.Fatalf("shared sessions changed saved source: %q, %v", data, err)
	}
	first.close()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatalf("shared daemon did not exit after its last client closed\n%s", daemonLog.String())
	}
}
