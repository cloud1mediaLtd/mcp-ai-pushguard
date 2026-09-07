package mcp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestMCPServer_Lifecycle(t *testing.T) {
	server := NewServer()

	// 1. Initialize
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n"
	input += `{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"
	input += `{"jsonrpc":"2.0","id":3,"method":"ping"}` + "\n"

	in := strings.NewReader(input)
	var out bytes.Buffer

	err := server.Serve(in, &out)
	if err != nil {
		t.Fatalf("Serve error: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 JSON-RPC responses, got %d:\n%s", len(lines), out.String())
	}

	// Verify initialize response
	var resp1 JSONRPCResponse
	if err := json.Unmarshal([]byte(lines[0]), &resp1); err != nil {
		t.Fatalf("failed to decode response 1: %v", err)
	}
	resMap, ok := resp1.Result.(map[string]any)
	if !ok || resMap["protocolVersion"] == nil {
		t.Errorf("expected protocolVersion in response 1")
	}

	// Verify tools/list response
	var resp2 JSONRPCResponse
	if err := json.Unmarshal([]byte(lines[1]), &resp2); err != nil {
		t.Fatalf("failed to decode response 2: %v", err)
	}
	toolsResult, ok := resp2.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected tools result in response 2")
	}
	tools, ok := toolsResult["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Errorf("expected tools list to have items, got %v", toolsResult)
	}

	// Verify ping response
	var resp3 JSONRPCResponse
	if err := json.Unmarshal([]byte(lines[2]), &resp3); err != nil {
		t.Fatalf("failed to decode response 3: %v", err)
	}
	if resp3.Error != nil {
		t.Errorf("expected no error for ping, got %v", resp3.Error)
	}
}
