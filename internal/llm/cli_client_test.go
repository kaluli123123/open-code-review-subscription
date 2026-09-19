// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const cliTestHelp = "--safe-mode --tools --strict-mcp-config --mcp-config --no-session-persistence --output-format --json-schema --system-prompt --setting-sources"
const cliTestResult = `{"type":"result","subtype":"success","is_error":false,"structured_output":{"content":"done","tool_calls":[]},"usage":{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":2,"cache_creation_input_tokens":4}}`

func cleanCLIEnv(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "ANTHROPIC_") || strings.HasPrefix(upper, "CLAUDE_CODE_") || strings.HasPrefix(upper, "CLAUDE_CONFIG_") || upper == "CLAUDE_API_KEY" || upper == "OCR_LLM_URL" || upper == "OCR_LLM_TOKEN" || upper == "OCR_LLM_AUTH_HEADER" || upper == "OCR_LLM_EXTRA_HEADERS" {
			t.Setenv(key, "")
		}
	}
}

func TestCLIExecutableHelper(t *testing.T) {
	start := -1
	for i, arg := range os.Args {
		if arg == "--ocr-cli-helper" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return
	}
	args := os.Args[start:]
	if len(args) == 1 && args[0] == "--help" {
		_, _ = os.Stdout.WriteString(cliTestHelp)
		os.Exit(0)
	}
	for i, arg := range args {
		if arg == "auth" && i+1 < len(args) && args[i+1] == "status" {
			_, _ = os.Stdout.WriteString(`{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty"}`)
			os.Exit(0)
		}
	}
	var req ChatRequest
	if json.NewDecoder(os.Stdin).Decode(&req) != nil {
		os.Exit(11)
	}
	if len(req.Messages) != 3 || req.Messages[1].ToolCalls[0].ID != "old" || req.Messages[2].ToolCallID != "old" {
		os.Exit(12)
	}
	entries, _ := os.ReadDir(".")
	if len(entries) != 0 {
		os.Exit(13)
	}
	for flag, value := range map[string]string{"--tools": "", "--mcp-config": `{"mcpServers":{}}`, "--setting-sources": "", "--output-format": "json"} {
		found := false
		for i, a := range args {
			if a == flag && i+1 < len(args) && args[i+1] == value {
				found = true
			}
		}
		if !found {
			os.Exit(14)
		}
	}
	_, _ = os.Stdout.WriteString(cliTestResult)
	os.Exit(0)
}
func TestCLIProcessAndRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process cleanup required")
	}
	cleanCLIEnv(t)
	dir := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quoted, _ := json.Marshal(binary)
	launcher := `package main
import("os";"os/exec")
func main(){c:=exec.Command(` + string(quoted) + `,append([]string{"-test.run=TestCLIExecutableHelper","--","--ocr-cli-helper"},os.Args[1:]...)...);c.Env=os.Environ();c.Stdin=os.Stdin;c.Stdout=os.Stdout;c.Stderr=os.Stderr;if c.Run()!=nil{os.Exit(1)}}`
	source := filepath.Join(dir, "main.go")
	if err = os.WriteFile(source, []byte(launcher), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "claude"), source)
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("build helper: %v %s", e, out)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	req := ChatRequest{MaxTokens: 8192, Messages: []Message{NewTextMessage("system", "review"), NewToolCallMessage("", []ToolCall{{ID: "old", Type: "function", Function: FunctionCall{Name: "read", Arguments: `{"path":"a"}`}}}, NativeTurn{Payload: make(chan int)}, ""), NewToolResultMessage("old", "file content")}}
	resp, err := NewCLIClient("claude-cli", ClientConfig{Timeout: 60 * time.Second}).CompletionsWithCtx(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content() != "done" || resp.Usage.TotalTokens != 19 {
		t.Fatalf("unexpected response: %+v", resp)
	}
}
func TestCLIResponseValidation(t *testing.T) {
	tool := ToolDef{Type: "function", Function: FunctionDef{Name: "read", Parameters: map[string]any{"type": "object", "required": []string{"path"}, "properties": map[string]any{"path": map[string]any{"type": "string"}}, "additionalProperties": false}}}
	call := `{"id":"call1","type":"function","function":{"name":"read","arguments":"{\"path\":\"a\"}"}}`
	withCall := strings.Replace(cliTestResult, `"tool_calls":[]`, `"tool_calls":[`+call+`]`, 1)
	for _, tc := range []struct {
		name, data string
		choice     string
		ok         bool
	}{
		{"text", cliTestResult, "auto", true},
		{"empty", strings.Replace(cliTestResult, `"content":"done"`, `"content":" "`, 1), "auto", false}, {"call", withCall, "required", true},
		{"none", withCall, "none", false}, {"required", cliTestResult, "required", false},
		{"missing parameter", strings.Replace(withCall, `"{\"path\":\"a\"}"`, `"{}"`, 1), "auto", false},
		{"wrong type", strings.Replace(withCall, `"{\"path\":\"a\"}"`, `"{\"path\":42}"`, 1), "auto", false},
		{"unknown parameter", strings.Replace(withCall, `"{\"path\":\"a\"}"`, `"{\"path\":\"a\",\"extra\":1}"`, 1), "auto", false},
		{"string arguments", strings.Replace(withCall, `"{\"path\":\"a\"}"`, `{}`, 1), "auto", false},
		{"duplicate id", strings.Replace(withCall, call, call+","+call, 1), "auto", false},
		{"unknown tool", strings.Replace(withCall, `"name":"read"`, `"name":"other"`, 1), "auto", false},
		{"error", strings.Replace(cliTestResult, `"is_error":false`, `"is_error":true`, 1), "auto", false},
		{"terminal", strings.Replace(cliTestResult, `"success"`, `"error_max_turns"`, 1), "auto", false},
		{"missing usage", strings.Replace(cliTestResult, `"input_tokens":10,`, "", 1), "auto", false},
		{"negative usage", strings.Replace(cliTestResult, `"input_tokens":10`, `"input_tokens":-1`, 1), "auto", false},
		{"unknown field", strings.Replace(cliTestResult, `"content":"done"`, `"content":"done","extra":1`, 1), "auto", false},
		{"malformed", `{`, "auto", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := parseCLIResponse([]byte(tc.data), ChatRequest{Tools: []ToolDef{tool}, ToolChoice: tc.choice})
			if (err == nil) != tc.ok {
				t.Fatalf("response=%+v error=%v", resp, err)
			}
		})
	}
}
func TestCLIRequestRejections(t *testing.T) {
	cleanCLIEnv(t)
	for _, tc := range []struct {
		req  ChatRequest
		want string
	}{
		{ChatRequest{ToolChoice: "bad"}, "unsupported CLI tool_choice"},
		{ChatRequest{ToolChoice: "required"}, "required tool_choice needs tools"},
		{ChatRequest{MaxTokens: -1}, "CLI max_tokens must not be negative"},
		{ChatRequest{Messages: []Message{{Role: "bad"}}}, "unsupported CLI message role"},
		{ChatRequest{Messages: []Message{{Role: "user", Content: 42}}}, "unsupported CLI message content"},
		{ChatRequest{Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "image"}}}}}, "unsupported CLI content block"},
	} {
		assertCLIError(t, validateCLIRequest(tc.req), tc.want)
	}
	_, err := NewCLIClient("unknown-cli", ClientConfig{}).CompletionsWithCtx(context.Background(), ChatRequest{})
	assertCLIError(t, err, "subscription CLI protocol unsupported")
	_, err = NewCLIClient("claude-cli", ClientConfig{APIKey: "secret"}).CompletionsWithCtx(context.Background(), ChatRequest{})
	assertCLIError(t, err, "does not accept API credentials")
	temp := 0.0
	_, err = NewCLIClient("claude-cli", ClientConfig{}).CompletionsWithCtx(context.Background(), ChatRequest{Temperature: &temp})
	assertCLIError(t, err, "does not support temperature")
	t.Setenv("ANTHROPIC_API_KEY", "do-not-leak")
	_, err = cliEnvironment()
	assertCLIError(t, err, "remove inherited Claude credential")
	if strings.Contains(err.Error(), "do-not-leak") {
		t.Fatal("unsafe environment handling")
	}
}

func assertCLIError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "context deadline exceeded") || strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected semantic error %q, got %v", want, err)
	}
}

func TestCLIBoundedOutput(t *testing.T) {
	var buf cliBoundedBuffer
	data := make([]byte, cliOutputLimit+1)
	n, err := buf.Write(data)
	if n != len(data) || err != nil || !buf.overflow || buf.Len() != cliOutputLimit {
		t.Fatal("output not bounded")
	}
}
func TestCLICancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := runCLI(ctx, "does-not-exist", nil, nil, t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestCLIRawToolDefinition(t *testing.T) {
	raw := json.RawMessage(`{"name":"read","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}`)
	var def FunctionDef
	if err := json.Unmarshal(raw, &def); err != nil {
		t.Fatal(err)
	}
	def.RawDefinition = raw
	req := ChatRequest{Tools: []ToolDef{{Type: "function", Function: def}}}
	if err := validateCLIRequest(req); err != nil {
		t.Fatal(err)
	}
	req.Tools[0].Function.Name = "different"
	if validateCLIRequest(req) == nil {
		t.Fatal("accepted inconsistent raw definition")
	}
}

func TestCLIUsageOverflow(t *testing.T) {
	data := strings.Replace(cliTestResult, `"input_tokens":10`, `"input_tokens":9223372036854775807`, 1)
	data = strings.Replace(data, `"cache_read_input_tokens":2`, `"cache_read_input_tokens":9223372036854775807`, 1)
	data = strings.Replace(data, `"cache_creation_input_tokens":4`, `"cache_creation_input_tokens":2`, 1)
	data = strings.Replace(data, `"output_tokens":3`, `"output_tokens":0`, 1)
	if _, err := parseCLIResponse([]byte(data), ChatRequest{}); err == nil {
		t.Fatal("accepted overflowed usage")
	}
}

// gateObservedContext signals that the completion has reached its queue wait.
type gateObservedContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *gateObservedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func TestCLIGateQueueAndCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixtures required")
	}
	for _, protocol := range []string{ProtocolClaudeCLI, ProtocolCodexCLI} {
		t.Run(protocol, func(t *testing.T) {
			cleanCLIEnv(t)
			gate := claudeCLIGate
			if protocol == ProtocolCodexCLI {
				gate = codexCLIGate
				installCodexFixture(t, "ok")
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			marker := filepath.Join(dir, "spawned")
			quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
			args := " -test.run=^TestCLIExecutableHelper$ -- --ocr-cli-helper"
			name := "claude"
			if protocol == ProtocolCodexCLI {
				args = " -test.run=^TestCodexFakeProcess$ -- --ocr-codex-fixture ok"
				name = "codex"
			}
			script := "#!/bin/sh\nprintf started > " + quote(marker) + "\nexec " + quote(exe) + args + " \"$@\"\n"
			if err = os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			req := ChatRequest{Messages: []Message{NewTextMessage("system", "review"), NewToolCallMessage("", []ToolCall{{ID: "old", Type: "function", Function: FunctionCall{Name: "read", Arguments: `{}`}}}, NativeTurn{}, ""), NewToolResultMessage("old", "file content")}}
			// Leave room for race-instrumented subprocess startup and exit.
			const executionTimeout = 10 * time.Second
			client := NewCLIClient(protocol, ClientConfig{Timeout: executionTimeout})
			for _, mode := range []string{"release", "cancel", "deadline"} {
				t.Run(mode, func(t *testing.T) {
					_ = os.Remove(marker)
					gate <- struct{}{}
					locked := true
					defer func() {
						if locked {
							<-gate
						}
					}()
					parent, cancel := context.WithCancel(context.Background())
					if mode == "deadline" {
						cancel()
						parent, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
					}
					defer cancel()
					ctx := &gateObservedContext{Context: parent, observed: make(chan struct{})}
					done := make(chan error, 1)
					go func() {
						resp, err := client.CompletionsWithCtx(ctx, req)
						if err == nil && (resp == nil || resp.Content() != "done") {
							err = errors.New("unexpected fixture response")
						}
						done <- err
					}()
					select {
					case <-ctx.observed:
					case err := <-done:
						t.Fatalf("returned before queue: %v", err)
					case <-time.After(5 * time.Second):
						t.Fatal("did not reach queue")
					}
					if mode == "release" {
						select {
						case err := <-done:
							t.Fatalf("queue consumed execution timeout: %v", err)
						case <-time.After(executionTimeout + 100*time.Millisecond):
						}
					} else if mode == "cancel" {
						cancel()
					}
					if _, err := os.Stat(marker); !os.IsNotExist(err) {
						t.Fatal("spawned while gate was held")
					}
					if mode == "release" {
						<-gate
						locked = false
					}
					select {
					case err := <-done:
						if mode == "release" && err != nil {
							t.Fatalf("failed after queue release: %v", err)
						}
						if mode != "release" && !errors.Is(err, parent.Err()) {
							t.Fatalf("expected caller cancellation: %v", err)
						}
					case <-time.After(executionTimeout + 5*time.Second):
						t.Fatal("completion did not finish")
					}
					if mode != "release" {
						if _, err := os.Stat(marker); !os.IsNotExist(err) {
							t.Fatal("spawned after caller cancellation")
						}
					}
				})
			}
		})
	}
}
