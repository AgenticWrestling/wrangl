// Package mcp is a small, dependency-free Model Context Protocol server core: JSON-RPC
// 2.0 with initialize, ping, tools/list and tools/call. Transports: Serve (newline-
// delimited stdio) and the HTTP handler in http.go.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// SupportedVersions are the MCP protocol revisions this server speaks (newest first).
var SupportedVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// Tool describes one tool. InputSchema is a JSON Schema object.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// CallFunc executes a tool. A returned error becomes an isError tool result (the model
// sees it), not a protocol error.
type CallFunc func(ctx context.Context, name string, args json.RawMessage) (any, error)

// Server answers MCP requests.
type Server struct {
	Name, Version string
	Tools         []Tool
	Call          CallFunc
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Handle processes one JSON-RPC message (or batch) and returns the encoded response, or
// nil for notifications.
func (s *Server) Handle(ctx context.Context, raw []byte) []byte {
	if len(raw) > 0 && raw[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(raw, &batch); err != nil {
			return enc(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
		}
		var out []json.RawMessage
		for _, m := range batch {
			if r := s.Handle(ctx, m); r != nil {
				out = append(out, r)
			}
		}
		if len(out) == 0 {
			return nil
		}
		b, _ := json.Marshal(out)
		return b
	}
	var req request
	if err := json.Unmarshal(raw, &req); err != nil || req.Method == "" {
		return enc(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
	}
	notification := len(req.ID) == 0
	result, rerr := s.dispatch(ctx, req)
	if notification {
		return nil
	}
	resp := response{JSONRPC: "2.0", ID: req.ID}
	if rerr != nil {
		resp.Error = rerr
	} else {
		resp.Result = result
	}
	return enc(resp)
}

func enc(v any) []byte { b, _ := json.Marshal(v); return b }

func (s *Server) dispatch(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		v := SupportedVersions[0]
		for _, sv := range SupportedVersions {
			if sv == p.ProtocolVersion {
				v = sv
			}
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]string{"name": s.Name, "version": s.Version},
		}, nil
	case "notifications/initialized", "notifications/cancelled":
		return nil, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.Tools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			return nil, &rpcError{-32602, "invalid params: name is required"}
		}
		known := false
		for _, t := range s.Tools {
			known = known || t.Name == p.Name
		}
		if !known {
			return nil, &rpcError{-32602, "unknown tool " + p.Name}
		}
		if len(p.Arguments) == 0 {
			p.Arguments = json.RawMessage("{}")
		}
		out, err := s.Call(ctx, p.Name, p.Arguments)
		if err != nil {
			return toolResult(err.Error(), true), nil
		}
		text, ok := out.(string)
		if !ok {
			b, _ := json.MarshalIndent(out, "", "  ")
			text = string(b)
		}
		return toolResult(text, false), nil
	}
	return nil, &rpcError{-32601, "method not found: " + req.Method}
}

func toolResult(text string, isErr bool) map[string]any {
	return map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": isErr}
}

// Serve speaks MCP over newline-delimited JSON on r/w (stdio) until r ends. Requests are
// handled concurrently (ask_question blocks for minutes); writes are serialised.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		if len(line) == 0 {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if out := s.Handle(ctx, line); out != nil {
				mu.Lock()
				fmt.Fprintf(w, "%s\n", out)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return sc.Err()
}

// Schema builds a JSON Schema object: props maps name -> JSON type ("string", "integer",
// "boolean", "array"); required lists required names.
func Schema(props map[string]string, required ...string) json.RawMessage {
	p := map[string]any{}
	for k, t := range props {
		switch t {
		case "array":
			p[k] = map[string]any{"type": "array", "items": map[string]any{}}
		case "string[]":
			p[k] = map[string]any{"type": "array", "items": map[string]string{"type": "string"}}
		default:
			p[k] = map[string]string{"type": t}
		}
	}
	if required == nil {
		required = []string{}
	}
	b, _ := json.Marshal(map[string]any{"type": "object", "properties": p, "required": required})
	return b
}
