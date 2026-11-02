//go:build !darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

type fileWatcher struct {
	root         string
	projectRoots []string
	watcher      *fsnotify.Watcher
	mu           sync.Mutex
	dirs         map[string]bool
	registered   map[string]bool
	files        map[string]bool
	pending      map[string]int
	err          error
	done         chan struct{}
	control      string
	marker       *os.File
	barrier      chan fsnotify.Op
	ctx          context.Context
	cancel       context.CancelFunc
}

func newFileWatcher(ctx context.Context, root string) (_ *fileWatcher, err error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	control, err := os.MkdirTemp("", "tern-go-watch-")
	if err != nil {
		w.Close()
		return nil, err
	}
	marker, err := os.OpenFile(filepath.Join(control, "barrier"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		w.Close()
		os.RemoveAll(control)
		return nil, err
	}
	f := &fileWatcher{watcher: w, dirs: make(map[string]bool), registered: make(map[string]bool), files: make(map[string]bool), pending: make(map[string]int), done: make(chan struct{}), control: control, barrier: make(chan fsnotify.Op, 16)}
	f.root = root
	f.ctx, f.cancel = context.WithCancel(context.Background())
	f.marker = marker
	defer func() {
		if err != nil {
			f.cancel()
			w.Close()
			marker.Close()
			os.RemoveAll(control)
		}
	}()
	// A durable marker gives each query a kernel-event delivery barrier.
	if err := w.Add(marker.Name()); err != nil {
		return nil, err
	}
	if err := f.addContext(ctx, root, true, false); err != nil {
		return nil, err
	}
	// Each CLI request resolves its workspace again. An ancestor go.work/go.mod
	// change that selects a different root gets a different daemon; watching
	// every ancestor would also watch unrelated files up to the filesystem root.
	go f.run()
	return f, nil
}

func (f *fileWatcher) prepareDiscovery(context.Context, string, []string) error {
	// inotify already watches a directory without opening each child.
	return nil
}

func (f *fileWatcher) add(root string, recursive, changed bool) error {
	return f.addContext(f.ctx, root, recursive, changed)
}

func (f *fileWatcher) addContext(ctx context.Context, root string, recursive, changed bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, err := f.addLocked(ctx, root, recursive, changed, false)
	return err
}

// addObserved adds a root discovered from module configuration and returns its
// key so discovery can release roots it no longer needs.
func (f *fileWatcher) addObserved(ctx context.Context, root string, recursive bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addLocked(ctx, root, recursive, false, false)
}

// addRegistered records roots requested by gopls's dynamic watch registration.
// Discovery must never prune them, even when module configuration changes.
func (f *fileWatcher) addRegistered(root string, recursive bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, err := f.addLocked(f.ctx, root, recursive, false, true)
	return err
}

func (f *fileWatcher) addLocked(ctx context.Context, root string, recursive, changed, registered bool) (string, error) {
	root = filepath.Clean(root)
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	if registered {
		f.registered[root] = true
	}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := f.ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			if path != root && (ignoredDir(entry.Name()) || !recursive) {
				return filepath.SkipDir
			}
			if already, ok := f.dirs[path]; ok && (already || !recursive) {
				return filepath.SkipDir
			}
			if err := f.watcher.Add(path); err != nil {
				return fmt.Errorf("watch %s: %w", path, err)
			}
			f.dirs[path] = recursive
		} else if relevantFile(path) {
			f.files[path] = true
			if changed {
				f.pending[path] = 1
			}
		}
		return nil
	})
	return root, err
}

// retainObserved releases watches and file states that the latest discovery no
// longer names. inotify spends a watch descriptor per directory, so retaining
// every replaced module or go.work member would grow without bound until the
// editor exits. LSP-registered roots and roots covering one are retained.
func (f *fileWatcher) retainObserved(ctx context.Context, desired map[string]bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	watched := make(map[string]bool, len(f.dirs))
	for dir, recursive := range f.dirs {
		watched[dir] = recursive
	}
	for dir := range watched {
		if f.dirWanted(dir, desired, watched) {
			continue
		}
		_ = f.watcher.Remove(dir)
		delete(f.dirs, dir)
	}
	for file := range f.files {
		if !f.watchedLocked(file) {
			delete(f.files, file)
		}
	}
	for file := range f.pending {
		if !f.watchedLocked(file) {
			delete(f.pending, file)
		}
	}
	return nil
}

func (f *fileWatcher) dirWanted(dir string, desired, watched map[string]bool) bool {
	if desired[dir] || f.registered[dir] {
		return true
	}
	inside := func(path, root string) bool {
		return path != root && strings.HasPrefix(path, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator))
	}
	for path := range desired {
		if watched[dir] && inside(path, dir) || watched[path] && inside(dir, path) {
			return true
		}
	}
	for path := range f.registered {
		if watched[dir] && inside(path, dir) || watched[path] && inside(dir, path) {
			return true
		}
	}
	return false
}

func (f *fileWatcher) watchedLocked(file string) bool {
	for dir, recursive := range f.dirs {
		if filepath.Dir(file) == dir || recursive && strings.HasPrefix(file, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (f *fileWatcher) run() {
	defer close(f.done)
	for {
		select {
		case event, ok := <-f.watcher.Events:
			if !ok {
				return
			}
			if event.Name == f.marker.Name() {
				select {
				case f.barrier <- event.Op:
				default:
				}
				continue
			}
			f.mu.Lock()
			name := filepath.Clean(event.Name)
			if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				if _, directory := f.dirs[name]; directory {
					for file := range f.files {
						if strings.HasPrefix(file, name+string(filepath.Separator)) {
							f.pending[file] = 3
							delete(f.files, file)
						}
					}
					for dir := range f.dirs {
						if dir == name || strings.HasPrefix(dir, name+string(filepath.Separator)) {
							_ = f.watcher.Remove(dir)
							delete(f.dirs, dir)
						}
					}
				}
				if relevantFile(name) {
					f.pending[name] = 3
					delete(f.files, name)
				}
			}
			if event.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(name); err == nil && info.IsDir() {
					// Only recursively extend recursive roots, not ancestors or
					// individually watched dependency package directories.
					if f.dirs[filepath.Dir(name)] && !ignoredDir(filepath.Base(name)) {
						if _, err := f.addLocked(f.ctx, name, true, true, false); err != nil {
							f.err = err
						}
					}
				} else if relevantFile(name) {
					f.pending[name] = 1
					f.files[name] = true
				}
			}
			if event.Op&fsnotify.Write != 0 && relevantFile(name) {
				if f.pending[name] != 1 {
					f.pending[name] = 2
				}
				f.files[name] = true
			}
			f.mu.Unlock()
		case err, ok := <-f.watcher.Errors:
			if !ok {
				return
			}
			f.mu.Lock()
			f.err = fmt.Errorf("filesystem watcher: %w", err)
			f.mu.Unlock()
		}
	}
}

func (f *fileWatcher) close() {
	f.cancel()
	_ = f.watcher.Close()
	<-f.done
	_ = f.marker.Close()
	_ = os.RemoveAll(f.control)
}

func (f *fileWatcher) changes(ctx context.Context) ([]fileChange, bool, error) {
	// Two round trips also drain the preceding backend event batch without
	// a fixed debounce interval or a workspace tree scan.
	for range 2 {
		if _, err := f.marker.WriteAt([]byte{1}, 0); err != nil {
			return nil, false, err
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-f.done:
			return nil, false, errors.New("filesystem watcher closed")
		case op := <-f.barrier:
			if op&fsnotify.Write == 0 {
				return nil, false, fmt.Errorf("unexpected watcher synchronization event: %s", op)
			}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, false, f.err
	}
	changes := make([]fileChange, 0, len(f.pending))
	modules := false
	for name, kind := range f.pending {
		// Atomic saves can arrive as create/remove in backend-specific order.
		// Inspect only event paths, never the workspace tree, to report the
		// final on-disk state rather than a transient rename intermediate.
		if _, err := os.Stat(name); err == nil {
			if kind == 3 {
				kind = 2
			}
			f.files[name] = true
		} else if os.IsNotExist(err) {
			kind = 3
			delete(f.files, name)
		} else {
			return nil, false, err
		}
		changes = append(changes, fileChange{URI: fileURI(name), Type: kind})
		switch filepath.Base(name) {
		case "go.mod", "go.sum", "go.work", "go.work.sum":
			modules = true
		}
		delete(f.pending, name)
	}
	return changes, modules, nil
}
