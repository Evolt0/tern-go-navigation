package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	requestTimeout = 45 * time.Second
	idleTimeout    = 5 * time.Minute
	maxQueryBytes  = 16 << 20
)

type daemonRequest struct {
	Query     query  `json:"query"`
	Workspace string `json:"workspace"`
	Shutdown  bool   `json:"shutdown,omitempty"`
}

type daemonResponse struct {
	Destinations []destination `json:"destinations"`
	Error        string        `json:"error,omitempty"`
}

func privateDir(path string) error {
	if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ok || stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("unsafe helper directory %s: must be owned by you with mode 0700", path)
	}
	return nil
}

func daemonDirectory(root string) (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	info, err := os.Stat(executable)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("cannot determine executable identity")
	}
	identity := fmt.Sprintf("%s\x00%d\x00%d\x00%d\x00%d\x00%s\x00%s", executable, stat.Dev, stat.Ino, info.Size(), info.ModTime().UnixNano(), os.Getenv("TERN_WINDOW_KEY"), root)
	for _, key := range []string{"GOPLS", "GOROOT", "GOPATH", "PATH", "GOWORK", "GOFLAGS", "GOTOOLCHAIN", "GOPROXY"} {
		identity += "\x00" + key + "=" + os.Getenv(key)
	}
	digest := sha256.Sum256([]byte(identity))
	// Darwin's per-user TMPDIR is often too long for sockaddr_un. The short
	// directory is protected against pre-created symlinks and other users.
	parent := filepath.Join("/tmp", "tern-go-"+strconv.Itoa(os.Getuid()))
	if err := privateDir(parent); err != nil {
		return "", err
	}
	dir := filepath.Join(parent, fmt.Sprintf("%x", digest[:16]))
	if err := privateDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

func lockFile(ctx context.Context, name string) (*os.File, error) {
	fd, err := syscall.Open(name, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			file.Close()
			return nil, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func dialDaemon(ctx context.Context, dir string, start bool) (net.Conn, error) {
	socket := filepath.Join(dir, "socket")
	dial := func() (net.Conn, error) { return (&net.Dialer{}).DialContext(ctx, "unix", socket) }
	if conn, err := dial(); err == nil {
		return conn, nil
	}
	if !start {
		return nil, nil
	}
	lock, err := lockFile(ctx, filepath.Join(dir, "startup.lock"))
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if conn, err := dial(); err == nil {
		return conn, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(filepath.Join(dir, "startup.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	ownerPID := 0
	if _, fromTern := os.LookupEnv("TERN_WINDOW_KEY"); fromTern {
		ownerPID = os.Getppid()
	}
	cmd := exec.Command(executable, "--daemon", dir, strconv.Itoa(ownerPID))
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	logFile.Close()
	if err != nil {
		return nil, fmt.Errorf("start navigation helper: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	for {
		if conn, err := dial(); err == nil {
			return conn, nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case err := <-exited:
			timer.Stop()
			log, _ := os.ReadFile(filepath.Join(dir, "startup.log"))
			return nil, fmt.Errorf("navigation helper exited: %v\n%s", err, log)
		case <-ctx.Done():
			timer.Stop()
			_ = cmd.Process.Kill()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func daemonQuery(ctx context.Context, q query, shutdown bool) ([]destination, error) {
	path, err := canonical(q.Path)
	if err != nil {
		return nil, err
	}
	root := workspace(path)
	dir, err := daemonDirectory(root)
	if err != nil {
		return nil, err
	}
	conn, err := dialDaemon(ctx, dir, !shutdown)
	if err != nil {
		return nil, err
	}
	if conn == nil {
		return []destination{}, nil
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := json.NewEncoder(conn).Encode(daemonRequest{Query: q, Workspace: root, Shutdown: shutdown}); err != nil {
		return nil, err
	}
	var response daemonResponse
	if err := json.NewDecoder(io.LimitReader(conn, 32<<20)).Decode(&response); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("navigation helper: %w", err)
	}
	if response.Error != "" {
		return nil, errors.New(response.Error)
	}
	return response.Destinations, nil
}

// Editor-owned sessions retain their in-memory index while the editor lives.
// Standalone CLI clients still get bounded idle lifetime.
func helperExpired(ownerPID int, inactive time.Duration) bool {
	if ownerPID > 1 {
		err := syscall.Kill(ownerPID, 0)
		return err != nil && !errors.Is(err, syscall.EPERM)
	}
	return inactive >= idleTimeout
}

func serveDaemon(dir string, ownerPID int) error {
	if err := privateDir(dir); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	version, err := os.Stat(executable)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// A lifetime lease prevents a stale socket from ever spawning two helpers.
	leaseCtx, leaseCancel := context.WithTimeout(ctx, requestTimeout)
	defer leaseCancel()
	lease, err := lockFile(leaseCtx, filepath.Join(dir, "lease.lock"))
	if err != nil {
		return err
	}
	defer lease.Close()
	socket := filepath.Join(dir, "socket")
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		return err
	}
	defer os.Remove(socket)
	var current *session
	defer func() { current.close() }()
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	var workers sync.WaitGroup
	var activityMu sync.Mutex
	lastActivity := time.Now()
	active := 0
	go func() {
		timer := time.NewTicker(time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = listener.Close()
				return
			case <-timer.C:
				latest, err := os.Stat(executable)
				replaced := err != nil || !os.SameFile(version, latest) || version.Size() != latest.Size() || !version.ModTime().Equal(latest.ModTime())
				activityMu.Lock()
				idle := active == 0 && (replaced || helperExpired(ownerPID, time.Since(lastActivity)))
				activityMu.Unlock()
				if idle {
					cancel()
					_ = listener.Close()
					return
				}
			}
		}
	}()
	// Bound simultaneous clients, including clients that never finish sending.
	clients := make(chan struct{}, 8)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() == nil {
				cancel()
			}
			break
		}
		select {
		case clients <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		activityMu.Lock()
		active++
		activityMu.Unlock()
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-clients; activityMu.Lock(); active--; lastActivity = time.Now(); activityMu.Unlock() }()
			defer conn.Close()
			requestCtx, requestCancel := context.WithTimeout(ctx, requestTimeout)
			defer requestCancel()
			deadline, _ := requestCtx.Deadline()
			_ = conn.SetDeadline(deadline)
			stop := context.AfterFunc(requestCtx, func() { _ = conn.Close() })
			defer stop()
			var request daemonRequest
			decoder := json.NewDecoder(io.LimitReader(conn, maxQueryBytes+1))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&request); err != nil {
				_ = json.NewEncoder(conn).Encode(daemonResponse{Error: err.Error()})
				return
			}
			// A closed CLI connection cancels queued work and an in-flight gopls
			// request immediately rather than retaining it for the entire timeout.
			go func() { var b [1]byte; _, _ = conn.Read(b[:]); requestCancel() }()
			select {
			case <-requestCtx.Done():
				return
			case <-gate:
			}
			defer func() { gate <- struct{}{} }()
			if request.Shutdown {
				current.close()
				current = nil
				_ = json.NewEncoder(conn).Encode(daemonResponse{Destinations: []destination{}})
				cancel()
				return
			}
			q, pos, err := prepareQuery(request.Query)
			if err != nil {
				_ = json.NewEncoder(conn).Encode(daemonResponse{Error: err.Error()})
				return
			}
			if q.Action == "warmup" && current != nil {
				_ = json.NewEncoder(conn).Encode(daemonResponse{Destinations: []destination{}})
				return
			}
			if err == nil {
				root := workspace(q.Path)
				if root != request.Workspace {
					err = fmt.Errorf("workspace changed during navigation: %s", root)
				}
				if err == nil && current != nil && current.root != root {
					err = errors.New("workspace does not match this helper")
				}
				if err == nil && current == nil {
					current, err = newSession(requestCtx, root)
				}
			}
			var destinations []destination
			if err == nil {
				destinations, err = current.navigate(requestCtx, q, pos)
			}
			response := daemonResponse{Destinations: destinations}
			if err != nil {
				current.close()
				current = nil
				if requestCtx.Err() != nil {
					err = requestCtx.Err()
				}
				response.Error = strings.TrimSpace(err.Error())
			}
			_ = json.NewEncoder(conn).Encode(response)
		}()
	}
	workers.Wait()
	return nil
}
