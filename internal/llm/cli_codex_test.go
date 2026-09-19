// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestCodexFakeProcess is an executable protocol fixture. It never contacts a
// service, reads credentials, or executes an OCR/native tool.
func TestCodexFakeProcess(t *testing.T) {
	marker := -1
	for i, a := range os.Args {
		if a == "--ocr-codex-fixture" {
			marker = i
			break
		}
	}
	if marker < 0 {
		return
	}
	scenario := os.Args[marker+1]
	for _, a := range os.Args[marker+2:] {
		if a == "--version" {
			fmt.Println("codex-cli 0.153.4")
			os.Exit(0)
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), cliOutputLimit)
	send := func(v any) {
		if json.NewEncoder(os.Stdout).Encode(v) != nil {
			os.Exit(3)
		}
	}
	notify := func(method string, p any) { send(map[string]any{"method": method, "params": p}) }
	seen := map[string]bool{}
	for scanner.Scan() {
		var q struct {
			ID     int            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &q) != nil {
			os.Exit(4)
		}
		reply := func(v any) { send(map[string]any{"id": q.ID, "result": v}) }
		fail := func() {
			send(map[string]any{"id": q.ID, "error": map[string]any{"code": -32602, "message": "fixture assertion failed"}})
		}
		switch q.Method {
		case "initialize":
			caps, _ := q.Params["capabilities"].(map[string]any)
			if caps["experimentalApi"] != true {
				fail()
				continue
			}
			reply(map[string]any{"userAgent": "codex-fixture"})
		case "initialized":
			seen[q.Method] = true
			notify("remoteControl/status/changed", map[string]any{})
		case "config/read":
			cfg := map[string]any{}
			for key, value := range codexIsolatedConfig() {
				parts := strings.Split(key, ".")
				at := cfg
				for _, part := range parts[:len(parts)-1] {
					child, ok := at[part].(map[string]any)
					if !ok {
						child = map[string]any{}
						at[part] = child
					}
					at = child
				}
				at[parts[len(parts)-1]] = value
			}
			layerTools := cfg["tools"]
			cfg["tools"] = map[string]any{"web_search": nil}
			cfg["chatgpt_base_url"] = "https://chatgpt.com/backend-api/"
			cfg["mcp_servers"] = map[string]any{"external": map[string]any{"enabled": true}, "web-search": map[string]any{"enabled": true}, `server.with"quote`: map[string]any{"enabled": true}}
			if scenario == "provider" {
				cfg["model_providers"] = map[string]any{"openai": map[string]any{"http_headers": map[string]any{"secret": "DO-NOT-PRINT"}}}
			}
			if scenario == "route" {
				cfg["chatgpt_base_url"] = "https://example.invalid"
			}
			if scenario == "ignored" {
				cfg["web_search"] = "live"
			}
			if scenario == "catalog" {
				cfg["model_catalog_json"] = "custom-catalog"
			}
			layers := []any{map[string]any{"name": map[string]any{"type": "sessionFlags"}, "config": map[string]any{"tools": layerTools}}}
			if scenario == "tools_projection" {
				layers = nil
			}
			origins := map[string]any{}
			for _, key := range []string{"tools.update_plan.enabled", "tools.experimental_request_user_input.enabled"} {
				origins[key] = map[string]any{"name": map[string]any{"type": "sessionFlags"}}
			}
			if scenario == "tools_origin" {
				origins = map[string]any{}
			}
			reply(map[string]any{"config": cfg, "layers": layers, "origins": origins})
		case "account/read":
			kind := "chatgpt"
			if scenario == "api" {
				kind = "apiKey"
			}
			reply(map[string]any{"account": map[string]any{"type": kind}, "requiresOpenaiAuth": true})
		case "mcpServerStatus/list":
			if q.Params["threadId"] == nil {
				reply(map[string]any{"data": []any{map[string]any{"name": "external"}}, "nextCursor": nil})
			} else if scenario == "mcp" {
				reply(map[string]any{"data": []any{map[string]any{"name": "external"}}, "nextCursor": nil})
			} else {
				tools := map[string]any{}
				if scenario == "mcp_disabled_tools" {
					tools["unexpected"] = map[string]any{}
				}
				reply(map[string]any{"data": []any{map[string]any{"name": "external", "runtimeStatus": "disabled", "tools": tools, "resources": []any{}, "resourceTemplates": []any{}}}, "nextCursor": nil})
			}
		case "thread/start":
			envs, ok := q.Params["environments"].([]any)
			cfg, _ := q.Params["config"].(map[string]any)
			servers, _ := cfg["mcp_servers"].(map[string]any)
			isolated := len(servers) == 3
			for _, name := range []string{"external", "web-search", `server.with"quote`} {
				entry, valid := servers[name].(map[string]any)
				isolated = isolated && valid && len(entry) == 1 && entry["enabled"] == false
			}
			// Mirror the real RPC behavior: TOML-quoted dotted keys create
			// a different server lacking a transport; they cannot disable it.
			for key := range cfg {
				if strings.HasPrefix(key, "mcp_servers.") {
					isolated = false
				}
			}
			if !ok || len(envs) != 0 || !isolated || q.Params["modelProvider"] != "openai" || q.Params["ephemeral"] != true || !seen["initialized"] {
				fail()
				continue
			}
			if scenario == "capability" {
				fail()
				continue
			}
			reply(map[string]any{"thread": map[string]any{"id": "thread-1"}, "modelProvider": "openai"})
			notify("deprecationNotice", map[string]any{})
		case "turn/start":
			envs, ok := q.Params["environments"].([]any)
			if !ok || len(envs) != 0 || q.Params["outputSchema"] == nil {
				fail()
				continue
			}
			if scenario == "early" {
				notify("turn/completed", map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "wrong-turn", "status": "completed"}})
			}
			reply(map[string]any{"turn": map[string]any{"id": "turn-1"}})
			if scenario == "request" {
				send(map[string]any{"id": "native-1", "method": "item/commandExecution/requestApproval", "params": map[string]any{}})
				continue
			}
			if scenario == "timeout" { // Stay blocked on stdin until the parent cancels us.
				continue
			}
			if scenario == "native" {
				notify("item/started", map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{"type": "commandExecution"}})
				continue
			}
			if scenario != "usage" {
				notify("thread/tokenUsage/updated", map[string]any{"threadId": "thread-1", "turnId": "turn-1", "tokenUsage": map[string]any{"total": map[string]any{"inputTokens": 12, "outputTokens": 4, "cachedInputTokens": 2, "totalTokens": 16}}})
			}
			text := `{"content":"done","tool_calls":[]}`
			if scenario == "tool" {
				text = `{"content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"path\":\"file.go\"}"}}]}`
			}
			notify("item/completed", map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{"type": "agentMessage", "phase": "final_answer", "text": text}})
			status := "completed"
			if scenario == "failed" {
				status = "failed"
			}
			notify("turn/completed", map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "status": status}})
		default:
			fail()
		}
	}
	os.Exit(0)
}

func installCodexFixture(t *testing.T, scenario string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nexec " + quote(exe) + " -test.run=^TestCodexFakeProcess$ -- --ocr-codex-fixture " + quote(scenario) + " \"$@\"\n"
	if err = os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "ANTHROPIC_") || strings.HasPrefix(key, "CLAUDE_CODE_") || strings.HasPrefix(key, "CLAUDE_CONFIG_") || strings.HasPrefix(key, "OPENAI_") || strings.HasPrefix(key, "CODEX_") || key == "CLAUDE_API_KEY" || key == "AZURE_OPENAI_API_KEY" {
			t.Setenv(key, "")
		}
	}
}
func TestCodexCLIProtocol(t *testing.T) {
	for _, scenario := range []string{"ok", "tool", "api", "mcp", "capability", "request", "native", "usage", "failed", "timeout", "provider", "route", "ignored", "early", "catalog", "tools_projection", "tools_origin", "mcp_disabled_tools"} {
		t.Run(scenario, func(t *testing.T) {
			installCodexFixture(t, scenario)
			cfg := ClientConfig{Model: "test-model", Timeout: 60 * time.Second}
			if scenario == "timeout" {
				cfg.Timeout = 150 * time.Millisecond
			}
			client := &codexCLIClient{cfg: cfg}
			req := ChatRequest{Messages: []Message{NewTextMessage("system", "review"), NewTextMessage("user", "input")}}
			if scenario == "tool" {
				req.ToolChoice = "required"
				req.Tools = []ToolDef{{Type: "function", Function: FunctionDef{Name: "lookup", Parameters: map[string]any{"type": "object", "required": []string{"path"}, "properties": map[string]any{"path": map[string]any{"type": "string"}}}}}}
			}
			resp, err := client.CompletionsWithCtx(context.Background(), req)
			if scenario != "ok" && scenario != "tool" {
				expected := map[string]string{
					"api": "official ChatGPT subscription login", "mcp": "still exposes MCP servers", "mcp_disabled_tools": "still exposes MCP servers",
					"capability": "thread/start failed", "request": "unauthorized native operation",
					"native": "used a native tool", "usage": "missing final output or valid token usage",
					"failed": "turn failed or was interrupted", "timeout": "context deadline exceeded",
					"provider": "custom official-provider overrides", "route": "chatgpt_base_url override",
					"ignored": "isolation configuration was not applied", "early": "early notification turn mismatch",
					"catalog": "model_catalog_json override", "tools_projection": "isolation configuration was not applied", "tools_origin": "tool isolation override is not authoritative",
				}[scenario]
				if err == nil || expected == "" || !strings.Contains(err.Error(), expected) {
					t.Fatalf("expected %q, got %v", expected, err)
				}
				if strings.Contains(err.Error(), "DO-NOT-PRINT") {
					t.Fatal("configuration leaked")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if resp.Usage == nil || resp.Usage.TotalTokens != 16 || resp.Usage.CacheReadTokens != 2 {
				t.Fatalf("usage: %+v", resp.Usage)
			}
			if scenario == "tool" {
				if len(resp.ToolCalls()) != 1 || resp.ToolCalls()[0].Function.Name != "lookup" {
					t.Fatal("missing OCR tool data")
				}
			} else if resp.Content() != "done" {
				t.Fatal("wrong content")
			}
		})
	}
}
func TestCodexRPCRejectsUnknownRequests(t *testing.T) {
	var out strings.Builder
	r := newCodexRPC(&out, strings.NewReader(""))
	err := r.notification(codexRPCMessage{ID: json.RawMessage(`"server-id"`), Method: "unknown/execute", Params: json.RawMessage(`{}`)})
	if err == nil || !strings.Contains(out.String(), "-32601") {
		t.Fatal("server request not refused")
	}
}
func TestCodexUsageValidation(t *testing.T) {
	n := func(i int64) *int64 { return &i }
	for _, u := range []codexUsage{{}, {Total: n(1), Input: n(2), Output: n(0), Cached: n(0)}, {Total: n(2), Input: n(1), Output: n(1), Cached: n(2)}, {Total: n(1), Input: n(1), Output: n(1), Cached: n(0)}} {
		if _, err := u.convert(); err == nil {
			t.Fatal("invalid usage accepted")
		}
	}
}
func TestCodexRPCMalformed(t *testing.T) {
	for _, data := range []string{"bad\n", `{"id":8,"result":{}}` + "\n", `{"id":1,"error":{"message":"secret"}}` + "\n"} {
		var out strings.Builder
		r := newCodexRPC(&out, strings.NewReader(data))
		if _, err := r.call("initialize", nil); err == nil {
			t.Fatal("malformed response accepted")
		}
	}
}
func TestCodexMCPPagination(t *testing.T) {
	data := `{"id":1,"result":{"data":[],"nextCursor":"repeat"}}` + "\n" + `{"id":2,"result":{"data":[],"nextCursor":"repeat"}}` + "\n"
	var out strings.Builder
	r := newCodexRPC(&out, strings.NewReader(data))
	if _, err := r.mcpNames(""); err == nil {
		t.Fatal("cursor loop accepted")
	}
}
func TestCodexConfigShape(t *testing.T) {
	cfg := codexIsolatedConfig()
	for _, key := range codexConfigKeys {
		if _, ok := cfg[key]; !ok {
			t.Fatal(key)
		}
	}
	if cfg["tools.experimental_request_user_input.enabled"] != false {
		t.Fatal(fmt.Sprint(cfg))
	}
}

func TestCodexNoEnvironmentVersion(t *testing.T) {
	for version, want := range map[string]bool{"codex-cli 0.153.4": true, "codex-cli 0.154.0": true, "codex-cli 0.153.3": false, "unknown": false} {
		if codexNoEnvironmentVersion(version) != want {
			t.Fatal(version)
		}
	}
}

func TestCodexMCPDisabledInventory(t *testing.T) {
	for _, tc := range []struct {
		name, entry string
		exposed     bool
	}{
		{"disabled-empty", `{"name":"fixture","runtimeStatus":"disabled","tools":{},"resources":[],"resourceTemplates":[]}`, false},
		{"enabled-empty", `{"name":"fixture","runtimeStatus":"ready","tools":{},"resources":[],"resourceTemplates":[]}`, true},
		{"missing-capabilities", `{"name":"fixture","runtimeStatus":"disabled"}`, true},
		{"unknown-state", `{"name":"fixture","tools":{},"resources":[],"resourceTemplates":[]}`, true},
		{"disabled-resource", `{"name":"fixture","runtimeStatus":"disabled","tools":{},"resources":[{}],"resourceTemplates":[]}`, true},
		{"disabled-template", `{"name":"fixture","runtimeStatus":"disabled","tools":{},"resources":[],"resourceTemplates":[{}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			data := `{"id":1,"result":{"data":[` + tc.entry + `],"nextCursor":null}}` + "\n"
			rpc := newCodexRPC(&out, strings.NewReader(data))
			names, err := rpc.mcpNames("thread")
			if err != nil || (len(names) > 0) != tc.exposed {
				t.Fatalf("exposed=%v, err=%v", len(names) > 0, err)
			}
		})
	}
}

func TestCodexPureStatusNotifications(t *testing.T) {
	for _, method := range []string{"thread/name/updated", "model/verification", "model/safetyBuffering/updated", "turn/moderationMetadata", "warning", "configWarning"} {
		t.Run(method, func(t *testing.T) {
			var out strings.Builder
			rpc := newCodexRPC(&out, strings.NewReader(""))
			err := rpc.notification(codexRPCMessage{Method: method, Params: json.RawMessage(`{"threadId":"thread","turnId":"turn","metadata":{"secret":"not-logged"}}`)})
			if err != nil {
				t.Fatal(err)
			}
			if out.Len() != 0 {
				t.Fatal("status notification produced a request")
			}
		})
	}
}
func TestCodexUnexpectedNotificationDiagnostics(t *testing.T) {
	for _, tc := range []struct{ method, want string }{
		{"unknown/status", "unknown/status"},
		{"model/rerouted", "model/rerouted"},
		{"item/commandExecution/outputDelta", "item/commandExecution/outputDelta"},
		{"secret\nBearer token", "[invalid-method]"},
		{strings.Repeat("x", 129), "[invalid-method]"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			var out strings.Builder
			rpc := newCodexRPC(&out, strings.NewReader(""))
			err := rpc.notification(codexRPCMessage{Method: tc.method, Params: json.RawMessage(`{"secret":"DO-NOT-PRINT"}`)})
			if err == nil || !strings.HasSuffix(err.Error(), tc.want) || strings.Contains(err.Error(), "DO-NOT-PRINT") {
				t.Fatalf("wrong diagnostic: %v", err)
			}
		})
	}
}

func TestCodexStrictResponseSchemaTypes(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(cliResponseSchema), &schema); err != nil {
		t.Fatal(err)
	}
	var check func(map[string]any)
	check = func(node map[string]any) {
		if _, ok := node["type"].(string); !ok {
			t.Fatal("strict output schema node lacks a type")
		}
		if properties, ok := node["properties"].(map[string]any); ok {
			for _, property := range properties {
				check(property.(map[string]any))
			}
		}
		if items, ok := node["items"].(map[string]any); ok {
			check(items)
		}
	}
	check(schema)
}
func TestCodexSafeErrorCategories(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"codexErrorInfo":"other","message":"{\"error\":{\"code\":\"invalid_json_schema\",\"message\":\"secret\"}}"}`, "invalid_json_schema"},
		{`{"codexErrorInfo":{"httpConnectionFailed":{"httpStatusCode":400}},"message":"secret"}`, "httpConnectionFailed"},
		{`{"codexErrorInfo":"usageLimitExceeded","message":"secret"}`, "usageLimitExceeded"},
		{`{"codexErrorInfo":"DO-NOT-PRINT","message":"secret"}`, "unknown"},
		{`{}`, "unknown"},
	} {
		if got := codexSafeError(json.RawMessage(tc.raw)); got != tc.want {
			t.Fatalf("got %q, want %q", got, tc.want)
		}
	}
}
