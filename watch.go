package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

type fileChange struct {
	URI  string `json:"uri"`
	Type int    `json:"type"`
}

type fileWatcher struct {
	root         string
	projectRoots []string
	watcher      *fsnotify.Watcher
	mu           sync.Mutex
	dirs         map[string]bool
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
	f := &fileWatcher{watcher: w, dirs: make(map[string]bool), files: make(map[string]bool), pending: make(map[string]int), done: make(chan struct{}), control: control, barrier: make(chan fsnotify.Op, 16)}
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
	// kqueue emits Create before installing its implicit file watch. Removing
	// that file on Create can therefore lose Remove forever. Watch one durable
	// marker explicitly before any query, then synchronize with single writes.
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

func relevantFile(path string) bool {
	switch filepath.Ext(path) {
	case ".go", ".mod", ".sum", ".work", ".c", ".h", ".s", ".S", ".syso":
		return true
	}
	return false
}

func ignoredDir(name string) bool {
	return name == ".git" || name == ".hg" || name == "node_modules"
}

func (f *fileWatcher) add(root string, recursive, changed bool) error {
	return f.addContext(f.ctx, root, recursive, changed)
}

func (f *fileWatcher) addContext(ctx context.Context, root string, recursive, changed bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addLocked(ctx, root, recursive, changed)
}

func (f *fileWatcher) addLocked(ctx context.Context, root string, recursive, changed bool) error {
	root = filepath.Clean(root)
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
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
						if err := f.addLocked(f.ctx, name, true, true); err != nil {
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
	// Two kernel-event round trips drain the backend's previous batch even on
	// kqueue, whose ready descriptors need not be returned in write order.
	// Unlike a sleep, this costs no fixed debounce interval or tree scan.
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

func (f *fileWatcher) register(raw json.RawMessage) error {
	var params struct {
		Registrations []struct {
			Method          string
			RegisterOptions struct {
				Watchers []struct{ GlobPattern json.RawMessage }
			}
		}
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return err
	}
	for _, registration := range params.Registrations {
		if registration.Method != "workspace/didChangeWatchedFiles" {
			continue
		}
		for _, watcher := range registration.RegisterOptions.Watchers {
			var pattern string
			if err := json.Unmarshal(watcher.GlobPattern, &pattern); err != nil {
				var relative struct {
					BaseURI json.RawMessage `json:"baseUri"`
					Pattern string
				}
				if err := json.Unmarshal(watcher.GlobPattern, &relative); err != nil {
					return err
				}
				var base string
				if err := json.Unmarshal(relative.BaseURI, &base); err != nil {
					var folder struct{ URI string }
					if err := json.Unmarshal(relative.BaseURI, &folder); err != nil {
						return err
					}
					base = folder.URI
				}
				u, err := url.Parse(base)
				if err != nil || u.Scheme != "file" {
					return fmt.Errorf("unsupported watch base URI %q", base)
				}
				pattern = filepath.Join(u.Path, relative.Pattern)
			}
			if !filepath.IsAbs(pattern) {
				pattern = filepath.Join(f.root, pattern)
			}
			prefix := pattern
			if i := strings.IndexAny(prefix, "*?{["); i >= 0 {
				prefix = prefix[:i]
			}
			root := filepath.Dir(prefix)
			if strings.HasSuffix(prefix, "/") {
				root = filepath.Clean(prefix)
			}
			if err := f.add(root, strings.Contains(pattern, "**"), false); err != nil {
				f.mu.Lock()
				f.err = err
				f.mu.Unlock()
				return err
			}
		}
	}
	return nil
}

// gopls watches active workspace modules, not every imported dependency.
// Explicit discovery preserves watches for local replacements and loaded
// dependency packages; it runs only on startup and module configuration changes.
func (f *fileWatcher) discover(ctx context.Context, root string, env []string) error {
	type module struct {
		Dir     string
		Main    bool
		Replace *struct {
			Dir     string
			Version string
		}
	}
	modules := make([]module, 0)
	// Keep main/local modules even when another dependency cannot be resolved.
	err := goJSON(ctx, root, env, []string{"list", "-m", "-e", "-json", "all"}, func(dec *json.Decoder) error {
		var m module
		if err := dec.Decode(&m); err != nil {
			return err
		}
		modules = append(modules, m)
		return nil
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// go list can fail for a broken workspace. Let gopls report its richer
	// workspace error rather than replacing it with a watcher setup error.
	_ = err
	projectRoots := make([]string, 0, len(modules))
	for _, m := range modules {
		if m.Replace != nil && m.Replace.Version == "" && m.Replace.Dir != "" {
			if err := f.addContext(ctx, m.Replace.Dir, true, false); err != nil {
				return err
			}
		}
		if !m.Main || m.Dir == "" {
			continue
		}
		moduleRoot, err := canonical(m.Dir)
		if err != nil {
			return err
		}
		projectRoots = append(projectRoots, moduleRoot)
		if err := f.addContext(ctx, m.Dir, true, false); err != nil {
			return err
		}
		_ = goJSON(ctx, m.Dir, env, []string{"list", "-deps", "-e", "-json", "./..."}, func(dec *json.Decoder) error {
			var p struct{ Dir string }
			if err := dec.Decode(&p); err != nil {
				return err
			}
			if p.Dir != "" {
				if err := f.addContext(ctx, p.Dir, false, false); err != nil {
					f.mu.Lock()
					f.err = err
					f.mu.Unlock()
					return err
				}
			}
			return nil
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if len(projectRoots) == 0 {
		projectRoots = []string{root}
	}
	f.projectRoots = projectRoots
	return nil
}

func goJSON(ctx context.Context, dir string, env []string, args []string, consume func(*json.Decoder) error) error {
	goPath := "go"
	for _, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			for _, directory := range filepath.SplitList(strings.TrimPrefix(entry, "PATH=")) {
				candidate := filepath.Join(directory, "go")
				if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
					goPath = candidate
					break
				}
			}
		}
	}
	cmd := exec.CommandContext(ctx, goPath, args...)
	cmd.Dir, cmd.Env = dir, env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	decoder := json.NewDecoder(stdout)
	for {
		err = consume(decoder)
		if err != nil {
			break
		}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if !errors.Is(err, io.EOF) {
		return err
	}
	return waitErr
}
