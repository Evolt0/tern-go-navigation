//go:build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/ebitengine/purego"
)

const (
	fsMustScanSubDirs = uint32(0x00000001)
	fsUserDropped     = uint32(0x00000002)
	fsKernelDropped   = uint32(0x00000004)
	fsIDsWrapped      = uint32(0x00000008)
	fsHistoryDone     = uint32(0x00000010)
	fsRootChanged     = uint32(0x00000020)
	fsMount           = uint32(0x00000040)
	fsUnmount         = uint32(0x00000080)
	fsItemCreated     = uint32(0x00000100)
	fsItemRemoved     = uint32(0x00000200)
	fsItemRenamed     = uint32(0x00000800)
	fsItemIsFile      = uint32(0x00010000)
	fsItemIsDir       = uint32(0x00020000)
	fsItemIsSymlink   = uint32(0x00040000)
)

// Native code retains only an integer token, never a Go pointer. The two
// callback trampolines are process-wide: purego callbacks cannot be freed.
var fseventsNative struct {
	once      sync.Once
	api       fseventsAPI
	err       error
	streams   sync.Map
	nextToken atomic.Uint64
	callback  uintptr
	barrier   uintptr
}

type fseventsAPI struct {
	currentID      func() uint64
	create         func(uintptr, uintptr, *fseventsContext, uintptr, uint64, float64, uint32) uintptr
	setQueue       func(uintptr, uintptr)
	start          func(uintptr) bool
	flush          func(uintptr)
	stop           func(uintptr)
	invalidate     func(uintptr)
	release        func(uintptr)
	stringCreate   func(uintptr, string, uint32) uintptr
	arrayCreate    func(uintptr, *uintptr, int64, uintptr) uintptr
	cfRelease      func(uintptr)
	arrayCallbacks uintptr
	queueCreate    func(string, uintptr) uintptr
	queueSync      func(uintptr, uintptr, uintptr)
	queueRelease   func(uintptr)
}

type fseventsContext struct {
	version         int64
	info            uintptr
	retain          uintptr
	release         uintptr
	copyDescription uintptr
}

type nativeFileEvent struct {
	path  string
	flags uint32
	id    uint64
}

type nativeEventStream struct {
	api       *fseventsAPI
	stream    uintptr
	queue     uintptr
	token     uintptr
	paths     []string
	started   bool
	control   string
	marker    string
	markerSeq uint64
	barrier   chan struct{}
	mu        sync.Mutex
	events    map[string]nativeFileEvent
	lastID    uint64
	err       error
	history   chan struct{}
	failed    chan struct{}
	historyOK bool
}

func loadFSEvents() (*fseventsAPI, error) {
	fseventsNative.once.Do(func() {
		// Successful library handles live with their function pointers for the
		// process lifetime; initialization failures close every opened handle.
		var handles []uintptr
		defer func() {
			if fseventsNative.err != nil {
				for i := len(handles) - 1; i >= 0; i-- {
					_ = purego.Dlclose(handles[i])
				}
			}
		}()
		open := func(path string) uintptr {
			if fseventsNative.err != nil {
				return 0
			}
			handle, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
			if err != nil {
				fseventsNative.err = fmt.Errorf("load filesystem event framework: %w", err)
				return 0
			}
			handles = append(handles, handle)
			return handle
		}
		fs := open("/System/Library/Frameworks/CoreServices.framework/Versions/A/Frameworks/FSEvents.framework/Versions/A/FSEvents")
		cf := open("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation")
		dispatch := open("/usr/lib/system/libdispatch.dylib")
		bind := func(target any, handle uintptr, name string) {
			if fseventsNative.err != nil {
				return
			}
			symbol, err := purego.Dlsym(handle, name)
			if err != nil {
				fseventsNative.err = fmt.Errorf("load %s: %w", name, err)
				return
			}
			purego.RegisterFunc(target, symbol)
		}
		a := &fseventsNative.api
		bind(&a.currentID, fs, "FSEventsGetCurrentEventId")
		bind(&a.create, fs, "FSEventStreamCreate")
		bind(&a.setQueue, fs, "FSEventStreamSetDispatchQueue")
		bind(&a.start, fs, "FSEventStreamStart")
		bind(&a.flush, fs, "FSEventStreamFlushSync")
		bind(&a.stop, fs, "FSEventStreamStop")
		bind(&a.invalidate, fs, "FSEventStreamInvalidate")
		bind(&a.release, fs, "FSEventStreamRelease")
		bind(&a.stringCreate, cf, "CFStringCreateWithCString")
		bind(&a.arrayCreate, cf, "CFArrayCreate")
		bind(&a.cfRelease, cf, "CFRelease")
		bind(&a.queueCreate, dispatch, "dispatch_queue_create")
		bind(&a.queueSync, dispatch, "dispatch_sync_f")
		bind(&a.queueRelease, dispatch, "dispatch_release")
		if fseventsNative.err != nil {
			return
		}
		var err error
		a.arrayCallbacks, err = purego.Dlsym(cf, "kCFTypeArrayCallBacks")
		if err != nil {
			fseventsNative.err = fmt.Errorf("load CF array callbacks: %w", err)
			return
		}
		fseventsNative.callback = purego.NewCallback(deliverFSEvents)
		fseventsNative.barrier = purego.NewCallback(func(uintptr) {})
	})
	return &fseventsNative.api, fseventsNative.err
}

func newNativeEventStream(api *fseventsAPI, paths []string, since uint64) (_ *nativeEventStream, err error) {
	s := &nativeEventStream{api: api, paths: paths, events: make(map[string]nativeFileEvent), lastID: since, history: make(chan struct{}), failed: make(chan struct{})}
	defer func() {
		if err != nil {
			s.close()
		}
	}()
	control, err := os.MkdirTemp("", "tern-go-fsevents-")
	if err != nil {
		return nil, err
	}
	s.control, err = canonical(control)
	if err != nil {
		_ = os.RemoveAll(control)
		return nil, err
	}
	s.queue = api.queueCreate("tern-go-navigation.fsevents", 0)
	if s.queue == 0 {
		return nil, errors.New("create filesystem event dispatch queue")
	}
	nativePaths := append(append(make([]string, 0, len(paths)+1), paths...), s.control)
	values := make([]uintptr, 0, len(nativePaths))
	defer func() {
		for _, value := range values {
			api.cfRelease(value)
		}
	}()
	for _, path := range nativePaths {
		value := api.stringCreate(0, path, 0x08000100) // kCFStringEncodingUTF8
		if value == 0 {
			return nil, fmt.Errorf("create filesystem event path %q", path)
		}
		values = append(values, value)
	}
	if len(values) == 0 {
		return nil, errors.New("filesystem event stream has no roots")
	}
	array := api.arrayCreate(0, &values[0], int64(len(values)), api.arrayCallbacks)
	if array == 0 {
		return nil, errors.New("create filesystem event path array")
	}
	defer api.cfRelease(array)
	s.token = uintptr(fseventsNative.nextToken.Add(1))
	fseventsNative.streams.Store(s.token, s)
	context := fseventsContext{info: s.token}
	// NoDefer | WatchRoot | FileEvents. A journal marker, rather than latency,
	// defines request boundaries. WatchRoot turns invalid paths into errors.
	s.stream = api.create(0, fseventsNative.callback, &context, array, since, 0, 0x02|0x04|0x10)
	if s.stream == 0 {
		return nil, errors.New("create filesystem event stream")
	}
	api.setQueue(s.stream, s.queue)
	if !api.start(s.stream) {
		return nil, errors.New("start filesystem event stream")
	}
	s.started = true
	return s, nil
}

// The callback only takes its inbox mutex. In particular it never takes the
// watcher/operation locks held by FlushSync, Stop, or the delivery barrier.
func deliverFSEvents(_ uintptr, token uintptr, count uintptr, paths unsafe.Pointer, flagsPointer *uint32, idsPointer *uint64) {
	value, ok := fseventsNative.streams.Load(token)
	if !ok {
		return
	}
	s := value.(*nativeEventStream)
	pathsSlice := unsafe.Slice((**byte)(paths), int(count))
	flags := unsafe.Slice(flagsPointer, int(count))
	ids := unsafe.Slice(idsPointer, int(count))
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, flag := range flags {
		if ids[i] > s.lastID {
			s.lastID = ids[i]
		}
		if flag&fsHistoryDone != 0 {
			if !s.historyOK {
				s.historyOK = true
				close(s.history)
			}
			continue
		}
		if flag&(fsMustScanSubDirs|fsUserDropped|fsKernelDropped|fsIDsWrapped|fsRootChanged|fsMount|fsUnmount) != 0 {
			s.failLocked(fmt.Errorf("filesystem event history is incomplete or a watched root changed (flags %#x); rebuild session", flag))
			continue
		}
		if s.err != nil {
			continue
		}
		pointer := pathsSlice[i]
		length := 0
		for *(*byte)(unsafe.Add(unsafe.Pointer(pointer), length)) != 0 {
			length++
		}
		// Borrowed only until the callback returns. Copy below only for paths
		// capable of affecting source files or directory membership.
		path := normalizeFSEventPath(unsafe.String(pointer, length))
		if darwinWithin(path, s.control) {
			if path == s.marker && s.barrier != nil {
				close(s.barrier)
				s.barrier = nil
			}
			continue
		}
		if flag&(fsItemIsFile|fsItemIsSymlink) != 0 && !relevantFile(path) {
			continue
		}
		if previous, ok := s.events[path]; ok {
			previous.flags |= flag
			if ids[i] > previous.id {
				previous.id = ids[i]
			}
			s.events[previous.path] = previous
			continue
		}
		if len(s.events) >= 65536 {
			s.failLocked(errors.New("filesystem event inbox overflow; rebuild session"))
			continue
		}
		path = strings.Clone(path)
		s.events[path] = nativeFileEvent{path: path, flags: flag, id: ids[i]}
	}
}

func normalizeFSEventPath(path string) string {
	// FSEvents may spell the standard macOS aliases differently from
	// EvalSymlinks. Resolve them lexically, including paths already deleted.
	for _, alias := range []string{"/var", "/tmp", "/etc"} {
		if path == alias || strings.HasPrefix(path, alias+"/") {
			return filepath.Clean("/private" + path)
		}
	}
	return filepath.Clean(path)
}

func (s *nativeEventStream) failLocked(err error) {
	if s.err == nil {
		s.err = err
		close(s.failed)
	}
}

func (s *nativeEventStream) sync(ctx, lifetime context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-lifetime.Done():
		return errors.New("filesystem watcher closed")
	case <-s.failed:
		s.mu.Lock()
		err := s.err
		s.mu.Unlock()
		return err
	case <-s.history:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := lifetime.Err(); err != nil {
		return errors.New("filesystem watcher closed")
	}
	// FlushSync alone need not include changes still waiting in the kernel.
	// A unique file in this same stream establishes an observable journal
	// boundary without sleeps, rescanning, or accepting a stale marker event.
	s.mu.Lock()
	s.markerSeq++
	marker := filepath.Join(s.control, fmt.Sprintf("barrier-%d", s.markerSeq))
	ready := make(chan struct{})
	s.marker, s.barrier = marker, ready
	s.mu.Unlock()
	if err := os.WriteFile(marker, []byte{1}, 0600); err != nil {
		return err
	}
	defer os.Remove(marker)
	s.api.flush(s.stream)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-lifetime.Done():
		return errors.New("filesystem watcher closed")
	case <-s.failed:
		s.mu.Lock()
		err := s.err
		s.mu.Unlock()
		return err
	case <-ready:
	}
	s.api.flush(s.stream)
	// FSEvents delivers on a serial dispatch queue. This barrier also drains
	// any callback work enqueued by FlushSync before we read the Go inbox.
	s.api.queueSync(s.queue, 0, fseventsNative.barrier)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := lifetime.Err(); err != nil {
		return errors.New("filesystem watcher closed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *nativeEventStream) drain() (map[string]nativeFileEvent, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	events := s.events
	if len(events) == 0 {
		return nil, s.lastID, s.err
	}
	s.events = make(map[string]nativeFileEvent)
	return events, s.lastID, s.err
}

func (s *nativeEventStream) close() {
	if s.stream != 0 {
		if s.started {
			s.api.stop(s.stream)
		}
		s.api.invalidate(s.stream)
		// Do not remove the registry token until all callbacks have exited.
		s.api.queueSync(s.queue, 0, fseventsNative.barrier)
		s.api.release(s.stream)
		s.api.queueSync(s.queue, 0, fseventsNative.barrier)
		s.stream = 0
	}
	if s.token != 0 {
		fseventsNative.streams.Delete(s.token)
		s.token = 0
	}
	if s.queue != 0 {
		s.api.queueRelease(s.queue)
		s.queue = 0
	}
	if s.control != "" {
		_ = os.RemoveAll(s.control)
	}
}
