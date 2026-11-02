package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type fileChange struct {
	URI  string `json:"uri"`
	Type int    `json:"type"`
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
			if err := f.addRegistered(root, strings.Contains(pattern, "**")); err != nil {
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
	desired := make(map[string]bool)
	observe := func(directory string, recursive bool) error {
		key, err := f.addObserved(ctx, directory, recursive)
		if err != nil {
			return err
		}
		if key != "" {
			desired[key] = true
		}
		return nil
	}
	if err := observe(root, true); err != nil {
		return err
	}
	projectRoots := make([]string, 0, len(modules))
	for _, m := range modules {
		if m.Replace != nil && m.Replace.Version == "" && m.Replace.Dir != "" {
			if err := observe(m.Replace.Dir, true); err != nil {
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
		if err := observe(m.Dir, true); err != nil {
			return err
		}
		var watchErr error
		_ = goJSON(ctx, m.Dir, env, []string{"list", "-deps", "-e", "-json", "./..."}, func(dec *json.Decoder) error {
			var p struct{ Dir string }
			if err := dec.Decode(&p); err != nil {
				return err
			}
			if p.Dir != "" {
				if err := observe(p.Dir, false); err != nil {
					watchErr = err
					return err
				}
			}
			return nil
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if watchErr != nil {
			return watchErr
		}
	}
	if len(projectRoots) == 0 {
		projectRoots = []string{root}
	}
	if err := f.prepareDiscovery(ctx, root, env); err != nil {
		return err
	}
	// A broken module listing is not evidence that previously discovered roots
	// disappeared, so only prune after a successful module enumeration.
	if len(modules) > 0 {
		if err := f.retainObserved(ctx, desired); err != nil {
			return err
		}
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
