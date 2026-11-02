//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

type darwinWatchRoot struct {
	path      string
	recursive bool
	since     uint64
}

type darwinFileState struct {
	size     int64
	modified int64
	mode     fs.FileMode
	inode    uint64
}

type fileWatcher struct {
	root         string
	projectRoots []string
	ctx          context.Context
	cancel       context.CancelFunc
	mu           sync.Mutex
	err          error

	// The cancellable operation gate serializes registration, synchronization,
	// and native teardown. Native callbacks only access their separate inbox.
	gate        chan struct{}
	closeOnce   sync.Once
	api         *fseventsAPI
	stream      *nativeEventStream
	roots       map[string]darwinWatchRoot
	registered  map[string]bool
	rootsDirty  bool
	nativeBases [2]string
	files       map[string]darwinFileState
	pending     map[string]int
}

func newFileWatcher(ctx context.Context, root string) (_ *fileWatcher, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	api, err := loadFSEvents()
	if err != nil {
		return nil, err
	}
	root, err = canonical(root)
	if err != nil {
		return nil, err
	}
	f := &fileWatcher{
		root: root, api: api, gate: make(chan struct{}, 1),
		roots: make(map[string]darwinWatchRoot), registered: make(map[string]bool),
		files: make(map[string]darwinFileState), pending: make(map[string]int),
	}
	f.ctx, f.cancel = context.WithCancel(context.Background())
	f.gate <- struct{}{}
	defer func() {
		if err != nil {
			f.close()
		}
	}()
	if err := f.addContext(ctx, root, true, false); err != nil {
		return nil, err
	}
	if err := f.acquire(ctx); err != nil {
		return nil, err
	}
	defer f.release()
	// Start before returning, not at the first query. All later registration
	// batches replay from an ID captured before taking their baseline snapshots.
	if err := f.configure(ctx); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *fileWatcher) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.ctx.Err(); err != nil {
		return errors.New("filesystem watcher closed")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.ctx.Done():
		return errors.New("filesystem watcher closed")
	case <-f.gate:
		if err := f.ctx.Err(); err != nil {
			f.release()
			return errors.New("filesystem watcher closed")
		}
		return nil
	}
}

func (f *fileWatcher) release() {
	f.gate <- struct{}{}
}

func (f *fileWatcher) watcherError() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *fileWatcher) fail(err error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err == nil {
		f.err = err
	}
	return f.err
}

func (f *fileWatcher) add(root string, recursive, changed bool) error {
	return f.addContext(f.ctx, root, recursive, changed)
}

func (f *fileWatcher) addContext(ctx context.Context, root string, recursive, changed bool) error {
	_, err := f.addRoot(ctx, root, recursive, changed, false)
	return err
}

// addObserved adds a root discovered from module configuration and returns its
// canonical key so discovery can release roots it no longer needs.
func (f *fileWatcher) addObserved(ctx context.Context, root string, recursive bool) (string, error) {
	return f.addRoot(ctx, root, recursive, false, false)
}

// addRegistered records roots requested by gopls's dynamic watch registration.
// Discovery must never prune them, even when module configuration changes.
func (f *fileWatcher) addRegistered(root string, recursive bool) error {
	_, err := f.addRoot(f.ctx, root, recursive, false, true)
	return err
}

func (f *fileWatcher) addRoot(ctx context.Context, root string, recursive, changed, registered bool) (string, error) {
	if err := f.acquire(ctx); err != nil {
		return "", err
	}
	defer f.release()
	if err := f.watcherError(); err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("watch root %s is not a directory", root)
	}
	root, err = canonical(root)
	if err != nil {
		return "", err
	}
	if registered {
		f.registered[root] = true
	}
	covered := false
	for _, existing := range f.roots {
		if existing.path == root && (existing.recursive || !recursive) || existing.recursive && darwinWithin(root, existing.path) && !darwinIgnoredBelow(root, existing.path, true) {
			if !changed {
				return root, nil
			}
			covered = true
		}
	}
	// Capture the journal cursor before enumerating. Reconfiguration may be
	// delayed until dependency discovery and LSP registration have both finished.
	since := f.api.currentID()
	baseline, err := f.snapshot(ctx, root, recursive)
	if err != nil {
		return "", err
	}
	for path, state := range baseline {
		if _, known := f.files[path]; !known {
			f.files[path] = state
		}
		if changed {
			f.pending[path] = 1
		}
	}
	if covered {
		return root, nil
	}
	if old, ok := f.roots[root]; ok && old.since < since {
		since = old.since
	}
	if recursive {
		for path, existing := range f.roots {
			if darwinWithin(path, root) && !darwinIgnoredBelow(path, root, true) {
				if existing.since < since {
					since = existing.since
				}
				delete(f.roots, path)
			}
		}
	}
	f.roots[root] = darwinWatchRoot{path: root, recursive: recursive, since: since}
	f.rootsDirty = true
	return root, nil
}

// retainObserved releases roots and file snapshots that the latest discovery no
// longer names. Without this, every local replacement or go.work change would
// keep a native watch root, its descriptors, and its file states until the
// editor exits. LSP-registered roots and any root covering one are retained.
func (f *fileWatcher) retainObserved(ctx context.Context, desired map[string]bool) error {
	if err := f.acquire(ctx); err != nil {
		return err
	}
	defer f.release()
	dropped := false
	for path, root := range f.roots {
		if f.rootWanted(root, desired) {
			continue
		}
		delete(f.roots, path)
		dropped = true
	}
	if !dropped {
		return nil
	}
	f.rootsDirty = true
	for path := range f.files {
		if !f.fileCovered(path) {
			delete(f.files, path)
		}
	}
	for path := range f.pending {
		if !f.fileCovered(path) {
			delete(f.pending, path)
		}
	}
	return f.configure(ctx)
}

func (f *fileWatcher) rootWanted(root darwinWatchRoot, desired map[string]bool) bool {
	if desired[root.path] || f.registered[root.path] {
		return true
	}
	if !root.recursive {
		return false
	}
	for path := range desired {
		if darwinWithin(path, root.path) && !darwinIgnoredBelow(path, root.path, true) {
			return true
		}
	}
	for path := range f.registered {
		if darwinWithin(path, root.path) && !darwinIgnoredBelow(path, root.path, true) {
			return true
		}
	}
	return false
}

func (f *fileWatcher) fileCovered(path string) bool {
	for _, root := range f.roots {
		if !darwinWithin(path, root.path) {
			continue
		}
		if root.recursive && !darwinIgnoredBelow(path, root.path, false) || filepath.Dir(path) == root.path {
			return true
		}
	}
	return false
}

func (f *fileWatcher) snapshot(ctx context.Context, root string, recursive bool) (map[string]darwinFileState, error) {
	files := make(map[string]darwinFileState)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := f.ctx.Err(); err != nil {
			return err
		}
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && (!recursive || ignoredDir(entry.Name())) {
				return filepath.SkipDir
			}
			return nil
		}
		if !relevantFile(path) {
			return nil
		}
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files[path] = darwinState(info)
		}
		return nil
	})
	return files, err
}

func darwinState(info os.FileInfo) darwinFileState {
	state := darwinFileState{size: info.Size(), modified: info.ModTime().UnixNano(), mode: info.Mode()}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		state.inode = stat.Ino
	}
	return state
}

func darwinWithin(path, root string) bool {
	if path == root {
		return true
	}
	if root == string(filepath.Separator) {
		return filepath.IsAbs(path)
	}
	return len(path) > len(root) && strings.HasPrefix(path, root) && path[len(root)] == filepath.Separator
}

func darwinIgnoredBelow(path, root string, directory bool) bool {
	if path == root {
		return false
	}
	relative := strings.TrimPrefix(strings.TrimPrefix(path, root), string(filepath.Separator))
	if !directory {
		relative = filepath.Dir(relative)
	}
	for relative != "" {
		part, rest, _ := strings.Cut(relative, string(filepath.Separator))
		if ignoredDir(part) {
			return true
		}
		relative = rest
	}
	return false
}

func (f *fileWatcher) prepareDiscovery(ctx context.Context, root string, env []string) error {
	var paths struct{ GOROOT, GOMODCACHE string }
	if err := goJSON(ctx, root, env, []string{"env", "-json", "GOROOT", "GOMODCACHE"}, func(dec *json.Decoder) error {
		return dec.Decode(&paths)
	}); err != nil {
		return err
	}
	var bases [2]string
	for i, path := range [2]string{paths.GOROOT, paths.GOMODCACHE} {
		if path == "" {
			continue
		}
		if i == 0 {
			path = filepath.Join(path, "src")
		}
		resolved, err := filepath.EvalSymlinks(path)
		if os.IsNotExist(err) {
			continue // A dependency-free workspace need not have a cache yet.
		}
		if err != nil {
			return err
		}
		bases[i] = resolved
	}
	if err := f.acquire(ctx); err != nil {
		return err
	}
	defer f.release()
	if f.nativeBases != bases {
		f.nativeBases = bases
		f.rootsDirty = true
	}
	return nil
}

func (f *fileWatcher) nativePaths() []string {
	paths := make([]string, 0, len(f.roots))
	for path := range f.roots {
		// WatchRoot opens descriptors for roots and their ancestors. Native
		// package-tree roots bound that cost; accepts still filters events to
		// the exact dependency packages registered by discovery.
		for _, base := range f.nativeBases {
			if base != "" && darwinWithin(path, base) {
				path = base
				break
			}
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	// FSEvents itself always watches recursively, even for nonrecursive
	// semantic roots. Do not spend native root slots on their descendants.
	result := paths[:0]
	for _, path := range paths {
		covered := false
		for _, parent := range result {
			if darwinWithin(path, parent) {
				covered = true
				break
			}
		}
		if !covered {
			result = append(result, path)
		}
	}
	return result
}

func (f *fileWatcher) configure(ctx context.Context) error {
	if f.stream != nil && !f.rootsDirty {
		return nil
	}
	paths := f.nativePaths()
	if f.stream != nil {
		unchanged := len(paths) == len(f.stream.paths)
		for i := range paths {
			if !unchanged || paths[i] != f.stream.paths[i] {
				unchanged = false
				break
			}
		}
		if unchanged {
			f.rootsDirty = false
			return nil
		}
		if err := f.stream.sync(ctx, f.ctx); err != nil {
			return err
		}
		old := f.stream
		// Stop before creating the replacement, keeping exactly one stream.
		// Events in this gap are recovered from the journal, not from a scan.
		old.close()
		f.stream = nil
		events, cursor, err := old.drain()
		if err != nil {
			return f.fail(err)
		}
		if err := f.applyEvents(ctx, events); err != nil {
			return f.fail(err)
		}
		for path, root := range f.roots {
			for _, nativePath := range old.paths {
				if darwinWithin(path, nativePath) {
					if cursor > root.since {
						root.since = cursor
						f.roots[path] = root
					}
					break
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.ctx.Err(); err != nil {
		return errors.New("filesystem watcher closed")
	}
	since := ^uint64(0)
	for _, root := range f.roots {
		if root.since < since {
			since = root.since
		}
	}
	stream, err := newNativeEventStream(f.api, paths, since)
	if err != nil {
		return f.fail(err)
	}
	f.stream = stream
	f.rootsDirty = false
	if err := stream.sync(ctx, f.ctx); err != nil {
		return err
	}
	return nil
}

func (f *fileWatcher) accepts(path string, directory bool, id uint64) bool {
	for _, root := range f.roots {
		if id <= root.since || !darwinWithin(path, root.path) || darwinIgnoredBelow(path, root.path, directory) {
			continue
		}
		if root.recursive || path == root.path || !directory && filepath.Dir(path) == root.path {
			return true
		}
	}
	return false
}

func (f *fileWatcher) applyEvents(ctx context.Context, events map[string]nativeFileEvent) error {
	// Reconcile directories first, then direct file events. A native batch can
	// include both sides of a rename; on-disk state decides their final meaning.
	for _, event := range events {
		if event.flags&fsItemIsDir == 0 && event.flags&(fsItemIsFile|fsItemIsSymlink) != 0 {
			continue
		}
		if event.flags&fsItemIsDir != 0 && event.flags&(fsItemCreated|fsItemRemoved|fsItemRenamed) == 0 {
			// Directory metadata changes are not membership changes. FileEvents
			// reports the actual child edits separately; do not walk the root.
			continue
		}
		if err := f.reconcileDirectory(ctx, event); err != nil {
			return err
		}
	}
	for path, event := range events {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !relevantFile(path) || !f.accepts(path, false, event.id) {
			continue
		}
		if err := f.reconcileFile(path, event.flags&fsItemCreated == 0); err != nil {
			return err
		}
	}
	return nil
}

func (f *fileWatcher) reconcileFile(path string, force bool) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) || err == nil && info.IsDir() {
		if _, known := f.files[path]; known {
			delete(f.files, path)
			f.pending[path] = 3
		}
		return nil
	}
	if err != nil {
		return err
	}
	state := darwinState(info)
	old, known := f.files[path]
	if !known {
		f.pending[path] = 1
	} else if force || old != state {
		if f.pending[path] != 1 {
			f.pending[path] = 2
		}
	}
	f.files[path] = state
	return nil
}

func (f *fileWatcher) reconcileDirectory(ctx context.Context, event nativeFileEvent) error {
	var scopes []darwinWatchRoot
	for _, root := range f.roots {
		if event.id <= root.since {
			continue
		}
		if darwinWithin(root.path, event.path) {
			scopes = append(scopes, root)
		} else if f.accepts(event.path, true, event.id) && darwinWithin(event.path, root.path) {
			scopes = append(scopes, darwinWatchRoot{path: event.path, recursive: root.recursive, since: root.since})
		}
	}
	for _, scope := range scopes {
		current, err := f.snapshot(ctx, scope.path, scope.recursive)
		if err != nil {
			return err
		}
		for path := range f.files {
			if darwinWithin(path, scope.path) && !darwinIgnoredBelow(path, scope.path, false) && (scope.recursive || filepath.Dir(path) == scope.path) {
				if _, exists := current[path]; !exists {
					f.pending[path] = 3
					delete(f.files, path)
				}
			}
		}
		for path, state := range current {
			old, known := f.files[path]
			if !known {
				f.pending[path] = 1
			} else if old != state || event.flags&fsItemIsDir == 0 {
				// Historical events can be directory-granular even with
				// FileEvents. Metadata alone cannot prove unchanged content.
				if f.pending[path] != 1 {
					f.pending[path] = 2
				}
			}
			f.files[path] = state
		}
	}
	return nil
}

func (f *fileWatcher) changes(ctx context.Context) ([]fileChange, bool, error) {
	if err := f.acquire(ctx); err != nil {
		return nil, false, err
	}
	defer f.release()
	if err := f.watcherError(); err != nil {
		return nil, false, err
	}
	if err := f.configure(ctx); err != nil {
		return nil, false, err
	}
	if err := f.stream.sync(ctx, f.ctx); err != nil {
		return nil, false, err
	}
	events, _, err := f.stream.drain()
	if err != nil {
		return nil, false, f.fail(err)
	}
	if err := f.applyEvents(ctx, events); err != nil {
		// A partially reconciled batch must never be silently forgotten on a
		// later request, including when the current request was cancelled.
		return nil, false, f.fail(err)
	}
	changes := make([]fileChange, 0, len(f.pending))
	modules := false
	for path, kind := range f.pending {
		info, err := os.Stat(path)
		if os.IsNotExist(err) || err == nil && info.IsDir() {
			kind = 3
			delete(f.files, path)
		} else if err != nil {
			return nil, false, f.fail(err)
		} else {
			if kind == 3 {
				kind = 2
			}
			f.files[path] = darwinState(info)
		}
		changes = append(changes, fileChange{URI: fileURI(path), Type: kind})
		switch filepath.Base(path) {
		case "go.mod", "go.sum", "go.work", "go.work.sum":
			modules = true
		}
	}
	clear(f.pending)
	return changes, modules, nil
}

func (f *fileWatcher) close() {
	f.closeOnce.Do(func() {
		f.cancel()
		<-f.gate
		if f.stream != nil {
			f.stream.close()
			f.stream = nil
		}
		f.release()
	})
}
