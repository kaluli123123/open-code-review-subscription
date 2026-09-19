// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package llmloop

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/llm"
	"github.com/alibaba/open-code-review/internal/tool"
)

// TestCLILoopHelperProcess is a local fake executable, never a model invocation.
func TestCLILoopHelperProcess(t *testing.T) {
	marker := -1
	for i, arg := range os.Args {
		if arg == "--" {
			marker = i
			break
		}
	}
	if marker < 0 {
		return
	}
	dir := os.Args[marker+1]
	args := os.Args[marker+2:]
	for _, arg := range args {
		if arg == "--help" {
			fmt.Print("--safe-mode --tools --strict-mcp-config --no-session-persistence --output-format --json-schema --model --print --permission-mode --setting-sources --disable-slash-commands --mcp-config --system-prompt")
			os.Exit(0)
		}
		if arg == "--version" {
			fmt.Print("2.1.276 (Claude Code)")
			os.Exit(0)
		}
	}
	if strings.Contains(strings.Join(args, " "), "auth status") {
		fmt.Print(`{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max","apiProvider":"firstParty"}`)
		os.Exit(0)
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	n := 0
	if data, err := os.ReadFile(filepath.Join(dir, "count")); err == nil {
		n, _ = strconv.Atoi(string(data))
	}
	n++
	if err := os.WriteFile(filepath.Join(dir, "count"), []byte(strconv.Itoa(n)), 0600); err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("request-%d.json", n)), input, 0600); err != nil {
		os.Exit(2)
	}
	data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("response-%d.json", n)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "unexpected extra completion")
		os.Exit(2)
	}
	fmt.Print(string(data))
	os.Exit(0)
}

func installCLILoopFake(t *testing.T, results ...any) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake executable uses a POSIX launcher")
	}
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	script := "#!/bin/sh\nexec " + quote(executable) + " -test.run='^TestCLILoopHelperProcess$' -- " + quote(dir) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	for i, result := range results {
		data, err := json.Marshal(map[string]any{
			"type": "result", "subtype": "success", "is_error": false,
			"session_id": fmt.Sprintf("fake-%d", i), "structured_output": result,
			"usage": map[string]int{"input_tokens": 20, "output_tokens": 10},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("response-%d.json", i+1)), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Only the fake executable is discoverable: a missing launcher cannot fall
	// through to the developer's real CLI or consume subscription credits.
	t.Setenv("PATH", dir)
	for _, key := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
		"ANTHROPIC_MODEL", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK",
		"CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "OPENAI_API_KEY",
		"OPENAI_BASE_URL", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN",
		"OCR_LLM_URL", "OCR_LLM_TOKEN", "OCR_LLM_PROTOCOL", "OCR_LLM_EXTRA_HEADERS",
	} {
		t.Setenv(key, "")
	}
	return dir
}

func cliLoopResult(name, id string, args map[string]any) map[string]any {
	encoded, _ := json.Marshal(args)
	return map[string]any{"content": "", "tool_calls": []any{map[string]any{
		"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(encoded)},
	}}}
}

func cliLoopToolDefs() []llm.ToolDef {
	defs := []llm.ToolDef{
		{Type: "function", Function: llm.FunctionDef{Name: "file_read", Description: "Read a file", Parameters: map[string]any{
			"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"},
		}}},
		{Type: "function", Function: llm.FunctionDef{Name: "task_done", Description: "Finish review", Parameters: map[string]any{
			"type": "object", "properties": map[string]any{},
		}}},
	}
	for i := range defs {
		defs[i].Function.RawDefinition, _ = json.Marshal(defs[i].Function)
	}
	return defs
}

func TestCLIClientRunsNativeOCRToolLoop(t *testing.T) {
	dir := installCLILoopFake(t,
		cliLoopResult("file_read", "read-1", map[string]any{"path": "main.go"}),
		cliLoopResult("task_done", "done-2", map[string]any{}),
	)
	client := llm.NewLLMClient(llm.ResolvedEndpoint{Protocol: "claude-cli", Model: "sonnet", Timeout: 60 * time.Second}, nil, nil)
	deps := newTestDeps(client)
	deps.Model = "sonnet"
	deps.MainToolDefs = cliLoopToolDefs()
	provider := &argsCapturingProvider{tool: tool.FileRead}
	reg := tool.NewRegistry()
	reg.Register(provider)
	deps.Tools = reg
	runner := NewRunner(deps)
	completed, stop, err := runner.RunMainTask(context.Background(), []llm.Message{
		llm.NewTextMessage("system", "Review with OCR tools."),
		llm.NewTextMessage("user", "Review main.go."),
	}, "main.go")
	if err != nil || !completed || stop != StopNone {
		t.Fatalf("completed=%v stop=%v err=%v", completed, stop, err)
	}
	if !provider.captured || provider.gotArgs["path"] != "main.go" {
		t.Fatalf("OCR tool was not executed with requested args: %#v", provider)
	}
	if runner.ToolCalls()["file_read"] != 1 {
		t.Fatalf("tool calls=%v", runner.ToolCalls())
	}
	if runner.TotalInputTokens() != 40 || runner.TotalOutputTokens() != 20 {
		t.Fatalf("unexpected usage input=%d output=%d", runner.TotalInputTokens(), runner.TotalOutputTokens())
	}
	data, err := os.ReadFile(filepath.Join(dir, "request-2.json"))
	if err != nil {
		t.Fatal(err)
	}
	var transcript any
	if err := json.Unmarshal(data, &transcript); err != nil {
		t.Fatalf("second request is not structured transcript: %v", err)
	}
	if !cliLoopHasToolResult(transcript, "read-1", "ok") {
		t.Fatalf("next transcript lost OCR tool result: %s", data)
	}
	count, err := os.ReadFile(filepath.Join(dir, "count"))
	if err != nil || string(count) != "2" {
		t.Fatalf("completion count=%s err=%v", count, err)
	}
}

func cliLoopHasToolResult(value any, id, content string) bool {
	switch v := value.(type) {
	case map[string]any:
		if v["role"] == "tool" && v["tool_call_id"] == id && v["content"] == content {
			return true
		}
		for _, child := range v {
			if cliLoopHasToolResult(child, id, content) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if cliLoopHasToolResult(child, id, content) {
				return true
			}
		}
	}
	return false
}

func TestCLIClientSupportsNativeTextOnlyAuxiliaryCall(t *testing.T) {
	dir := installCLILoopFake(t, map[string]any{"content": "Compact review context", "tool_calls": []any{}})
	client := llm.NewLLMClient(llm.ResolvedEndpoint{Protocol: "claude-cli", Model: "sonnet", Timeout: 60 * time.Second}, nil, nil)
	response, err := client.CompletionsWithCtx(context.Background(), llm.ChatRequest{
		Model: "sonnet", Messages: []llm.Message{llm.NewTextMessage("user", "Summarize the review context.")}, ToolChoice: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.VisibleContent() != "Compact review context" || len(response.ToolCalls()) != 0 {
		t.Fatalf("unexpected text-only response: %#v", response)
	}
	data, err := os.ReadFile(filepath.Join(dir, "request-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Fatal("auxiliary request must preserve structured transcript")
	}
}
