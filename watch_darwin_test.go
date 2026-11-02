//go:build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Readdirnames avoids os.ReadDir's fallback lstat of transient /dev/fd entries.
// Verify each numeric descriptor with F_GETFD after closing our directory FD,
// so the measurement itself contributes no open descriptor to the result.
func watcherOpenDescriptors(t *testing.T) int {
	t.Helper()
	dir, err := os.Open("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	names, readErr := dir.Readdirnames(-1)
	closeErr := dir.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	count := 0
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
			count++
		} else if !errors.Is(err, unix.EBADF) {
			t.Fatalf("probe descriptor %d: %v", fd, err)
		}
	}
	return count
}

func watcherWarmDescriptorFacilities(t *testing.T) {
	t.Helper()
	// Explicitly initialize runtime netpoll/deadline machinery before measuring
	// anything. An already-expired deadline requires no timing-dependent sleep.
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	if err := reader.SetReadDeadline(time.Now()); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := reader.Read(b[:]); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("initialize deadline machinery: %v", err)
	}

	// Warm native framework initialization and a complete delivery/close cycle,
	// too: one-time backend resources are not a per-watcher descriptor leak.
	root := watcherTestRoot(t)
	file := filepath.Join(root, "warm.go")
	putFile(t, file, "package warm\n")
	w, err := newFileWatcher(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	putFile(t, file, "package warm\n\nvar Ready = true\n")
	watcherTestChanges(t, w, false, file)
}

func TestWatcherDarwinDescriptorFootprint(t *testing.T) {
	watcherWarmDescriptorFacilities(t)
	root := watcherTestRoot(t)
	for i := range 2048 {
		putFile(t, filepath.Join(root, fmt.Sprintf("unrelated-%04d.txt", i)), "not Go source\n")
	}
	file := filepath.Join(root, "source.go")
	putFile(t, file, "package source\n")
	before := watcherOpenDescriptors(t)
	w, err := newFileWatcher(context.Background(), root)
	if err != nil {
		t.Fatalf("watching a directory containing 2048 unrelated files: %v", err)
	}
	defer w.close()
	putFile(t, file, "package source\n\nvar Immediate = true\n")
	watcherTestChanges(t, w, false, file)
	after := watcherOpenDescriptors(t)
	// Allow fixed stream/control descriptors and incidental runtime activity,
	// but not a descriptor per file (or even a small fraction of the 2048).
	if delta := after - before; delta > 32 {
		t.Errorf("watcher opened %d descriptors for 2048 unrelated files; want at most 32 (before %d, after %d)", delta, before, after)
	}
}

func TestWatcherDarwinDescriptorLifecycle(t *testing.T) {
	watcherWarmDescriptorFacilities(t)
	root := watcherTestRoot(t)
	for i := range 8 {
		putFile(t, filepath.Join(root, fmt.Sprintf("source-%d.go", i)), "package source\n")
	}
	file := filepath.Join(root, "source-0.go")
	before := watcherOpenDescriptors(t)
	for i := range 12 {
		func() {
			w, err := newFileWatcher(context.Background(), root)
			if err != nil {
				t.Fatalf("initialize watcher %d: %v", i, err)
			}
			defer w.close()
			putFile(t, file, fmt.Sprintf("package source\n\nvar Revision = %d\n", i))
			watcherTestChanges(t, w, false, file)
		}()
	}
	after := watcherOpenDescriptors(t)
	// Eight descriptors tolerate incidental process-wide runtime activity, but
	// cannot hide even one leaked descriptor per lifecycle over twelve cycles.
	if delta := after - before; delta > 8 {
		t.Errorf("successful watcher lifecycles leaked %d descriptors (before %d, after %d); tolerance 8", delta, before, after)
	}

	// Measure failed initialization separately, so successful-close leaks do not
	// obscure whether the cancellation cleanup path releases its own resources.
	before = watcherOpenDescriptors(t)
	for i := range 12 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		w, err := newFileWatcher(ctx, root)
		if err == nil {
			w.close()
			t.Fatalf("initialization %d with a canceled context succeeded", i)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("initialization %d: want context.Canceled, got %v", i, err)
		}
	}
	after = watcherOpenDescriptors(t)
	if delta := after - before; delta > 8 {
		t.Errorf("canceled watcher initializations leaked %d descriptors (before %d, after %d); tolerance 8", delta, before, after)
	}
}

func TestWatcherDarwinFailedSessionReleasesDescriptors(t *testing.T) {
	watcherWarmDescriptorFacilities(t)
	root := watcherTestRoot(t)
	// A broken module has no dependencies to discover, but startup must still
	// release its already-created watcher when the language server cannot start.
	putFile(t, filepath.Join(root, "go.mod"), "invalid go.mod\n")
	for i := range 8 {
		putFile(t, filepath.Join(root, fmt.Sprintf("source-%d.go", i)), "package source\n")
	}
	t.Setenv("GOPLS", filepath.Join(root, "missing-gopls"))
	t.Setenv("GOROOT", runtime.GOROOT())
	t.Setenv("PATH", filepath.Join(runtime.GOROOT(), "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	t.Setenv("GOPROXY", "off")
	failSession := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s, err := newSession(ctx, root)
		if err == nil {
			s.close()
			t.Fatal("session unexpectedly started a nonexistent language server")
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("want missing language server error, got %v", err)
		}
	}
	// Warm subprocess/pipe facilities as well, before measuring repeated
	// consumer-visible session failures rather than just direct watcher Close.
	failSession()
	before := watcherOpenDescriptors(t)
	for range 12 {
		failSession()
	}
	after := watcherOpenDescriptors(t)
	if delta := after - before; delta > 8 {
		t.Errorf("failed session initialization leaked %d descriptors (before %d, after %d); tolerance 8", delta, before, after)
	}
}

func TestWatcherDarwinDependencyRootFootprintAndScope(t *testing.T) {
	watcherWarmDescriptorFacilities(t)
	root, cache := watcherTestRoot(t), watcherTestRoot(t)
	t.Setenv("GOMODCACHE", cache)
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	t.Setenv("GOPROXY", "off")
	putFile(t, filepath.Join(root, "go.mod"), "module example.com/watchfootprint\n\ngo 1.23\n")
	putFile(t, filepath.Join(root, "source.go"), "package watchfootprint\n\nimport \"fmt\"\n\nvar Format = fmt.Sprint\n")
	env, err := goEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	before := watcherOpenDescriptors(t)
	w, err := newFileWatcher(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := w.discover(ctx, root, env); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(cache, "example.com", "dependency@v1.0.0")
	direct := filepath.Join(external, "direct.go")
	nested := filepath.Join(external, "nested", "excluded.go")
	unregistered := filepath.Join(cache, "other@v1.0.0", "excluded.go")
	putFile(t, direct, "package dependency\n")
	putFile(t, nested, "package excluded\n")
	putFile(t, unregistered, "package excluded\n")
	if err := w.add(external, false, false); err != nil {
		t.Fatal(err)
	}
	putFile(t, direct, "package dependency\n\nvar Updated = true\n")
	putFile(t, nested, "package excluded\n\nvar Excluded = true\n")
	putFile(t, unregistered, "package excluded\n\nvar Excluded = true\n")
	// Broader native cache watches must not broaden package-level semantics.
	watcherTestChanges(t, w, false, direct)
	if delta := watcherOpenDescriptors(t) - before; delta > 96 {
		t.Errorf("discovered fmt dependencies opened %d descriptors; want at most 96", delta)
	}
}
