package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Queries are serialized by the session. The reader stays alive between queries
// so server requests and diagnostics can never fill gopls's stdout pipe.
type rpc struct {
	in          *bufio.Reader
	out         io.Writer
	writeMu     sync.Mutex
	mu          sync.Mutex
	serverError string
	readError   error
	id          int
	responses   chan message
	done        chan struct{}
	register    func(json.RawMessage) error
}

func newRPC(in io.Reader, out io.Writer, register func(json.RawMessage) error) *rpc {
	r := &rpc{in: bufio.NewReader(in), out: out, responses: make(chan message, 1), done: make(chan struct{}), register: register}
	go r.run()
	return r
}

func (r *rpc) send(value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	_, err = fmt.Fprintf(r.out, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return err
}

func (r *rpc) receive() (message, error) {
	length := -1
	headerBytes := 0
	for {
		line, err := r.in.ReadString('\n')
		if err != nil {
			return message{}, err
		}
		headerBytes += len(line)
		if headerBytes > 8192 {
			return message{}, fmt.Errorf("oversized LSP header")
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(key, "Content-Length") {
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return message{}, err
			}
		}
	}
	if length < 0 || length > 32<<20 {
		return message{}, fmt.Errorf("invalid LSP Content-Length: %d", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r.in, body); err != nil {
		return message{}, err
	}
	var result message
	err := json.Unmarshal(body, &result)
	return result, err
}

func (r *rpc) run() {
	defer close(r.done)
	for {
		m, err := r.receive()
		if err != nil {
			r.mu.Lock()
			r.readError = err
			r.mu.Unlock()
			return
		}
		if m.Method == "" {
			select {
			case r.responses <- m:
			default:
				r.mu.Lock()
				r.readError = fmt.Errorf("unexpected extra LSP response")
				r.mu.Unlock()
				return
			}
			continue
		}
		if len(m.ID) == 0 {
			if m.Method == "window/showMessage" {
				var p struct {
					Type    int
					Message string
				}
				if json.Unmarshal(m.Params, &p) == nil && p.Type == 1 {
					r.mu.Lock()
					r.serverError = p.Message
					r.mu.Unlock()
				}
			}
			continue
		}
		var result any
		var requestErr error
		switch m.Method {
		case "workspace/configuration":
			var p struct{ Items []json.RawMessage }
			requestErr = json.Unmarshal(m.Params, &p)
			result = make([]any, len(p.Items))
		case "client/registerCapability":
			if r.register != nil {
				requestErr = r.register(m.Params)
			}
		case "client/unregisterCapability", "window/workDoneProgress/create":
		default:
			requestErr = fmt.Errorf("unsupported client method: %s", m.Method)
		}
		response := map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result}
		if requestErr != nil {
			delete(response, "result")
			response["error"] = map[string]any{"code": -32603, "message": requestErr.Error()}
		}
		if err := r.send(response); err != nil {
			r.mu.Lock()
			r.readError = err
			r.mu.Unlock()
			return
		}
	}
}

func (r *rpc) notify(method string, params any) error {
	return r.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (r *rpc) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	r.id++
	id := r.id
	if err := r.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		// The owner terminates this session on cancellation, including servers
		// that stop reading their input. The next request gets a fresh session.
		return nil, ctx.Err()
	case <-r.done:
		r.mu.Lock()
		defer r.mu.Unlock()
		return nil, fmt.Errorf("gopls connection closed: %w; %s", r.readError, r.serverError)
	case m := <-r.responses:
		if string(m.ID) != strconv.Itoa(id) {
			return nil, fmt.Errorf("unexpected LSP response id %s", m.ID)
		}
		if m.Error != nil {
			r.mu.Lock()
			detail := r.serverError
			r.mu.Unlock()
			return nil, fmt.Errorf("%s: %s\n%s", method, m.Error.Message, detail)
		}
		return m.Result, nil
	}
}
