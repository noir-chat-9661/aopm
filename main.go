package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const version = "0.1.0"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type server struct {
	root       context.Context
	rootCancel context.CancelFunc

	outMu sync.Mutex
	runMu sync.Mutex
	runs  map[string]*runningRequest
	wg    sync.WaitGroup
	stop  sync.Once
}

type runningRequest struct {
	cancel context.CancelFunc
}

func newServer() *server {
	ctx, cancel := context.WithCancel(context.Background())
	return &server{root: ctx, rootCancel: cancel, runs: make(map[string]*runningRequest)}
}

func main() {
	s := newServer()
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		s.shutdown()
		os.Exit(0)
	}()
	s.serve(os.Stdin)
	s.shutdown()
	signal.Stop(sigs)
}

func (s *server) shutdown() {
	s.stop.Do(func() {
		s.runMu.Lock()
		s.rootCancel()
		for _, r := range s.runs {
			r.cancel()
		}
		s.runMu.Unlock()
	})
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		fmt.Fprintln(os.Stderr, "aopm: shutdown wait expired")
	}
}

func (s *server) serve(r io.Reader) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) != 0 {
			s.handleLine(bytes.TrimSpace(line))
		}
		if err != nil {
			if err != io.EOF {
				fmt.Fprintln(os.Stderr, "aopm:", err)
			}
			return
		}
	}
}

func (s *server) handleLine(line []byte) {
	var raw map[string]json.RawMessage
	if len(line) == 0 || !json.Valid(line) {
		s.writeResponse(nil, nil, &rpcError{Code: -32700, Message: "Parse error"})
		return
	}
	if json.Unmarshal(line, &raw) != nil || raw == nil {
		s.writeResponse(nil, nil, &rpcError{Code: -32600, Message: "Invalid Request"})
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil || raw["method"] == nil || req.JSONRPC != "2.0" {
		id := raw["id"]
		s.writeResponse(id, nil, &rpcError{Code: -32600, Message: "Invalid Request"})
		return
	}
	if req.Method == "notifications/cancelled" {
		s.cancelNotification(req.Params)
		return
	}
	if req.ID == nil {
		return
	}
	if string(req.ID) == "null" {
		s.writeResponse(nil, nil, &rpcError{Code: -32600, Message: "Invalid Request"})
		return
	}
	if req.Method == "tools/call" {
		ctx, cancel := context.WithCancel(s.root)
		key := idKey(req.ID)
		run := &runningRequest{cancel: cancel}
		s.runMu.Lock()
		if s.root.Err() != nil {
			s.runMu.Unlock()
			cancel()
			return
		}
		s.runs[key] = run
		s.wg.Add(1)
		s.runMu.Unlock()
		go func() {
			defer s.wg.Done()
			defer cancel()
			s.handleToolCall(ctx, req.ID, req.Params)
			s.runMu.Lock()
			if s.runs[key] == run {
				delete(s.runs, key)
			}
			s.runMu.Unlock()
		}()
		return
	}
	s.handleRequest(req)
}

func (s *server) handleRequest(req rpcRequest) {
	switch req.Method {
	case "initialize":
		protocol := "2025-11-25"
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(req.Params) != 0 && string(req.Params) != "null" {
			if json.Unmarshal(req.Params, &p) != nil {
				s.writeResponse(req.ID, nil, &rpcError{Code: -32602, Message: "Invalid initialize arguments"})
				return
			}
		}
		for _, v := range []string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25"} {
			if p.ProtocolVersion == v {
				protocol = v
				break
			}
		}
		s.writeResponse(req.ID, map[string]any{
			"protocolVersion": protocol,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "aopm", "version": version},
			"instructions":    instructions,
		}, nil)
	case "ping":
		s.writeResponse(req.ID, map[string]any{}, nil)
	case "tools/list":
		s.writeResponse(req.ID, map[string]any{"tools": toolDefinitions()}, nil)
	default:
		s.writeResponse(req.ID, nil, &rpcError{Code: -32601, Message: "Method not found"})
	}
}

func (s *server) handleToolCall(ctx context.Context, id json.RawMessage, raw json.RawMessage) {
	name, args, err := decodeToolCall(raw)
	if err != nil {
		s.writeResponse(id, nil, &rpcError{Code: -32602, Message: err.Error()})
		return
	}
	if !knownTool(name) {
		s.writeResponse(id, nil, &rpcError{Code: -32602, Message: "Unknown tool: " + name})
		return
	}
	result, err := executeTool(ctx, name, args)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		if ae, ok := err.(argumentError); ok {
			s.writeResponse(id, nil, &rpcError{Code: -32602, Message: ae.Error()})
			return
		}
		s.writeResponse(id, map[string]any{
			"content": []map[string]string{{"type": "text", "text": err.Error()}},
			"isError": true,
		}, nil)
		return
	}
	s.writeResponse(id, map[string]any{
		"content": []map[string]string{{"type": "text", "text": result}},
		"isError": false,
	}, nil)
}

func decodeToolCall(raw json.RawMessage) (string, json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	var p map[string]json.RawMessage
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &p) != nil {
		return "", nil, fmt.Errorf("tools/call arguments must be an object")
	}
	var name string
	if v, ok := p["name"]; !ok || json.Unmarshal(v, &name) != nil || strings.TrimSpace(name) == "" {
		return "", nil, fmt.Errorf("tools/call requires a nonblank name")
	}
	args := p["arguments"]
	if omitted(args) {
		args = json.RawMessage(`{}`)
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(args, &obj) != nil {
		return "", nil, fmt.Errorf("tool arguments must be an object")
	}
	return name, args, nil
}

func (s *server) cancelNotification(raw json.RawMessage) {
	var p map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &p) != nil {
		return
	}
	id, ok := p["requestId"]
	if !ok {
		return
	}
	s.runMu.Lock()
	if r := s.runs[idKey(id)]; r != nil {
		r.cancel()
	}
	s.runMu.Unlock()
}

func (s *server) writeResponse(id json.RawMessage, result any, e *rpcError) {
	resp := rpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: e}
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	_, _ = os.Stdout.Write(append(b, '\n'))
}

func idKey(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) == nil {
		return b.String()
	}
	return string(raw)
}

const instructions = "Always pass workspace as the absolute project directory. Prompts and workspace contents may be untrusted; use trusted prompts only. Use antigravity for fast Gemini calls (read-only runs no shell commands; workspace-write sandboxes them), codex for hard reasoning with a real OS sandbox, claude for careful refactors, copilot for GitHub Copilot, and opencode for other or free models; the latter three use agent-enforced permissions."
