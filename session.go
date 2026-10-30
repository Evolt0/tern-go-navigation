package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type overlay struct {
	text    string
	version int
}

type session struct {
	root         string
	projectRoots []string
	env          []string
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	rpc          *rpc
	watcher      *fileWatcher
	documents    map[string]overlay
	closeOnce    sync.Once
	stderr       boundedLog
}

func newSession(ctx context.Context, root string) (_ *session, err error) {
	s := &session{root: root, documents: make(map[string]overlay)}
	defer func() {
		if err != nil {
			s.close()
			err = fmt.Errorf("%w\n%s", err, s.stderr.String())
		}
	}()
	s.env, err = goEnvironment()
	if err != nil {
		return nil, err
	}
	s.watcher, err = newFileWatcher(ctx, root)
	if err != nil {
		return nil, err
	}
	if err := s.watcher.discover(ctx, root, s.env); err != nil {
		return nil, err
	}
	s.projectRoots = s.watcher.projectRoots
	gopls := os.Getenv("GOPLS")
	if gopls == "" {
		gopls, err = exec.LookPath("gopls")
		if err != nil {
			home, homeErr := os.UserHomeDir()
			if homeErr != nil {
				return nil, homeErr
			}
			gopls = filepath.Join(home, "go", "bin", "gopls")
		}
	}
	s.cmd = exec.Command(gopls, "serve")
	s.cmd.Dir, s.cmd.Env, s.cmd.Stderr = root, s.env, &s.stderr
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	s.cmd.WaitDelay = time.Second
	stdout, err := s.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	s.stdin, err = s.cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := s.cmd.Start(); err != nil {
		return nil, fmt.Errorf("start gopls (install it or set GOPLS): %w", err)
	}
	// Kill unresponsive initialization on the request deadline. The successful
	// session itself has daemon lifetime, not the lifetime of its first query.
	stop := context.AfterFunc(ctx, func() { s.watcher.cancel(); s.kill() })
	defer stop()
	s.rpc = newRPC(stdout, s.stdin, s.watcher.register)
	uri := fileURI(root)
	result, err := s.rpc.call(ctx, "initialize", map[string]any{
		"processId": os.Getpid(), "rootUri": uri,
		"workspaceFolders": []any{map[string]any{"uri": uri, "name": filepath.Base(root)}},
		"capabilities": map[string]any{
			"general":   map[string]any{"positionEncodings": []string{"utf-16"}},
			"workspace": map[string]any{"didChangeWatchedFiles": map[string]any{"dynamicRegistration": true, "relativePatternSupport": true}},
		},
		"initializationOptions": map[string]any{
			"diagnosticsDelay": "1h", "checkUpdates": "off", "symbolScope": "workspace",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("initialize gopls: %w", err)
	}
	var init struct {
		Capabilities struct{ PositionEncoding string }
	}
	if err := json.Unmarshal(result, &init); err != nil {
		return nil, err
	}
	if enc := init.Capabilities.PositionEncoding; enc != "" && enc != "utf-16" {
		return nil, fmt.Errorf("unsupported gopls position encoding %q", enc)
	}
	if err := s.rpc.notify("initialized", map[string]any{}); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *session) close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.kill()
		if s.stdin != nil {
			_ = s.stdin.Close()
		}
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Wait()
		}
		if s.watcher != nil {
			s.watcher.close()
		}
	})
}

func (s *session) kill() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		_ = s.cmd.Process.Kill()
	}
}

// Preserve useful startup/workspace diagnostics without growing a daemon log
// indefinitely or retaining unbounded language-server stderr in memory.
type boundedLog struct {
	mu   sync.Mutex
	data []byte
}

func (b *boundedLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	const limit = 8192
	if len(p) >= limit {
		b.data = append(b.data[:0], p[len(p)-limit:]...)
		return n, nil
	}
	if excess := len(b.data) + len(p) - limit; excess > 0 {
		copy(b.data, b.data[excess:])
		b.data = b.data[:len(b.data)-excess]
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (b *boundedLog) String() string { b.mu.Lock(); defer b.mu.Unlock(); return string(b.data) }

func (s *session) syncDocuments(documents map[string]string) error {
	for name := range s.documents {
		if _, ok := documents[name]; !ok {
			if err := s.rpc.notify("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": fileURI(name)}}); err != nil {
				return err
			}
			delete(s.documents, name)
		}
	}
	for name, text := range documents {
		previous, open := s.documents[name]
		if open && previous.text == text {
			continue
		}
		version := previous.version + 1
		var err error
		if open {
			err = s.rpc.notify("textDocument/didChange", map[string]any{
				"textDocument":   map[string]any{"uri": fileURI(name), "version": version},
				"contentChanges": []any{map[string]any{"text": text}},
			})
		} else {
			err = s.rpc.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{
				"uri": fileURI(name), "languageId": "go", "version": version, "text": text,
			}})
		}
		if err != nil {
			return err
		}
		s.documents[name] = overlay{text: text, version: version}
	}
	return nil
}

func (s *session) navigate(ctx context.Context, q query, pos position) (_ []destination, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w\n%s", err, s.stderr.String())
		}
	}()
	// Cancellation closes pipes as well as terminating gopls, so writes and reads
	// remain bounded even when the language server is wedged.
	stop := context.AfterFunc(ctx, func() { s.watcher.cancel(); s.kill(); _ = s.stdin.Close() })
	defer stop()
	changes, modules, err := s.watcher.changes(ctx)
	if err != nil {
		return nil, err
	}
	if len(changes) != 0 {
		if err := s.rpc.notify("workspace/didChangeWatchedFiles", map[string]any{"changes": changes}); err != nil {
			return nil, err
		}
	}
	if modules {
		if err := s.watcher.discover(ctx, s.root, s.env); err != nil {
			return nil, err
		}
		s.projectRoots = s.watcher.projectRoots
	}
	if q.Action == "symbols" {
		for name := range q.Documents {
			included, err := projectSymbolDirectory(filepath.Dir(name), s.projectRoots)
			if err != nil {
				return nil, err
			}
			if !included {
				delete(q.Documents, name)
			}
		}
	}
	if err := s.syncDocuments(q.Documents); err != nil {
		return nil, err
	}
	if q.Action == "symbols" {
		return s.symbols(ctx, q)
	}
	if q.Action == "warmup" {
		_, err := s.rpc.call(ctx, "textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": fileURI(q.Path)}})
		return []destination{}, err
	}
	params := map[string]any{"textDocument": map[string]any{"uri": fileURI(q.Path)}, "position": pos}
	out := make([]destination, 0)
	seen := make(map[string]bool)
	for _, method := range []string{"definition", "references"} {
		kind := "definition"
		if method == "references" {
			params["context"] = map[string]any{"includeDeclaration": false}
			kind = "reference"
		}
		result, err := s.rpc.call(ctx, "textDocument/"+method, params)
		if err != nil {
			return nil, err
		}
		var locations []location
		if len(result) > 0 && result[0] == '{' {
			var single location
			if err := json.Unmarshal(result, &single); err != nil {
				return nil, err
			}
			locations = []location{single}
		} else if err := json.Unmarshal(result, &locations); err != nil {
			return nil, err
		}
		targets, err := destinations(locations, kind, q, seen)
		if err != nil {
			return nil, err
		}
		out = append(out, targets...)
	}
	return out, nil
}
