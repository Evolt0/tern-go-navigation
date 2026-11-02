package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func watcherTestRoot(t *testing.T) string {
	t.Helper()
	root, err := canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func watcherTestChanges(t *testing.T, w *fileWatcher, wantModules bool, paths ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	changes, modules, err := w.changes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if modules != wantModules {
		t.Errorf("module configuration changed = %v, want %v; changes: %+v", modules, wantModules, changes)
	}
	want := make(map[string]bool, len(paths))
	for _, path := range paths {
		want[fileURI(path)] = true
	}
	for _, change := range changes {
		if !want[change.URI] {
			t.Errorf("unexpected change: %+v", change)
		}
		delete(want, change.URI)
		if change.Type != 1 && change.Type != 2 {
			t.Errorf("existing file reported with change type %d: %s", change.Type, change.URI)
		}
	}
	for uri := range want {
		t.Errorf("missing change for %s; changes: %+v", uri, changes)
	}
}

func TestWatcherPopulatedDirectoryMovedIntoRecursiveRoot(t *testing.T) {
	base := watcherTestRoot(t)
	root := filepath.Join(base, "workspace")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	incoming := filepath.Join(base, "incoming")
	putFile(t, filepath.Join(incoming, "package.go"), "package incoming\n")
	putFile(t, filepath.Join(incoming, "nested", "source.go"), "package nested\n")
	w, err := newFileWatcher(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	moved := filepath.Join(root, "imported")
	if err := os.Rename(incoming, moved); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(moved, "nested", "source.go")
	// No consumer drain occurs between the directory move and this edit.
	putFile(t, source, "package nested\n\nvar Immediate = true\n")
	watcherTestChanges(t, w, false, filepath.Join(moved, "package.go"), source)
	// A later edit must also be tracked, not merely discovered in the move.
	putFile(t, source, "package nested\n\nvar Later = true\n")
	watcherTestChanges(t, w, false, source)
}

func TestWatcherNonrecursiveExternalPackageScope(t *testing.T) {
	root := watcherTestRoot(t)
	external := watcherTestRoot(t)
	direct := filepath.Join(external, "dependency.go")
	nested := filepath.Join(external, "nested", "other.go")
	putFile(t, direct, "package dependency\n")
	putFile(t, nested, "package other\n")
	w, err := newFileWatcher(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	if err := w.add(external, false, false); err != nil {
		t.Fatal(err)
	}
	putFile(t, direct, "package dependency\n\nvar Immediate = true\n")
	putFile(t, nested, "package other\n\nvar Excluded = true\n")
	watcherTestChanges(t, w, false, direct)

	created := filepath.Join(external, "created.go")
	putFile(t, created, "package dependency\n")
	putFile(t, filepath.Join(external, "new-package", "excluded.go"), "package excluded\n")
	watcherTestChanges(t, w, false, created)
	// Newly created direct files must remain observable after their creation.
	putFile(t, created, "package dependency\n\nvar Updated = true\n")
	putFile(t, nested, "package other\n\nvar StillExcluded = true\n")
	watcherTestChanges(t, w, false, created)
}

func TestWatcherIgnoredSubtreeChangesExcluded(t *testing.T) {
	root := watcherTestRoot(t)
	ignored := []string{".git", ".hg", "node_modules"}
	for _, dir := range ignored {
		putFile(t, filepath.Join(root, dir, "existing.go"), "package ignored\n")
	}
	visible := filepath.Join(root, "visible.go")
	putFile(t, visible, "package visible\n")
	w, err := newFileWatcher(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	for _, dir := range ignored {
		putFile(t, filepath.Join(root, dir, "existing.go"), "package ignored\n\nvar Excluded = true\n")
		putFile(t, filepath.Join(root, dir, "nested", "created.go"), "package ignored\n")
	}
	putFile(t, visible, "package visible\n\nvar First = true\n")
	watcherTestChanges(t, w, false, visible)

	// Ignored names must also be excluded when first created after registration.
	for _, dir := range ignored {
		putFile(t, filepath.Join(root, "new", dir, "created.go"), "package ignored\n")
	}
	putFile(t, visible, "package visible\n\nvar Second = true\n")
	watcherTestChanges(t, w, false, visible)
}

func TestWatcherModuleConfigurationFlag(t *testing.T) {
	root := watcherTestRoot(t)
	files := []struct {
		name    string
		modules bool
	}{
		{"source.go", false},
		{"ordinary.mod", false},
		{"go.mod", true},
		{"go.sum", true},
		{"go.work", true},
		{"go.work.sum", true},
	}
	for _, file := range files {
		putFile(t, filepath.Join(root, file.name), "before\n")
	}
	w, err := newFileWatcher(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	for _, file := range files {
		t.Run(file.name, func(t *testing.T) {
			path := filepath.Join(root, file.name)
			putFile(t, path, "after\n")
			watcherTestChanges(t, w, file.modules, path)
		})
	}
	// The configuration flag describes a drained batch, not a sticky state.
	watcherTestChanges(t, w, false)
}
