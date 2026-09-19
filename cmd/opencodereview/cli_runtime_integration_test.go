// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/llm"
)

// This exercises the actual default tool loader and agent.BuildToolDefs, not a
// reduced test definition. RawDefinition must remain acceptable to the CLI.
func TestCLIRuntimeAcceptsActualMainAndPlanDefinitions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake executable uses a POSIX launcher")
	}
	setTestHome(t, t.TempDir())
	dir := t.TempDir()
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	requestPath := filepath.Join(dir, "request.json")
	script := `#!/bin/sh
case "$*" in
 *--help*) printf '%s' '--safe-mode --tools --strict-mcp-config --mcp-config --no-session-persistence --output-format --json-schema --system-prompt --setting-sources'; exit 0;;
 *'auth status'*) printf '%s' '{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","subscriptionType":"max"}'; exit 0;;
esac
/bin/cat > ` + quote(requestPath) + `
printf '%s' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"content":"Ready for the next OCR phase","tool_calls":[]},"usage":{"input_tokens":12,"output_tokens":7}}'
`
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for _, key := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL",
		"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
		"OCR_LLM_PROTOCOL", "OCR_LLM_URL", "OCR_LLM_TOKEN", "OCR_LLM_MODEL", "OCR_LLM_AUTH_HEADER",
		"OCR_LLM_EXTRA_HEADERS", "OCR_LLM_TIMEOUT", "OCR_USE_ANTHROPIC",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("OCR_LLM_TIMEOUT", "60")
	rt, err := loadLLMRuntime(loadTestTemplate(t), "", llm.ResolveOptions{Provider: "claude-cli", Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	if rt.RuntimeConfig.Protocol != "claude-cli" || rt.Client == nil {
		t.Fatalf("unexpected runtime: %#v", rt)
	}
	for phase, defs := range map[string][]llm.ToolDef{"plan": rt.PlanToolDefs, "main": rt.MainToolDefs} {
		t.Run(phase, func(t *testing.T) {
			if len(defs) == 0 {
				t.Fatal("expected actual embedded tool definitions")
			}
			for _, def := range defs {
				if len(def.Function.RawDefinition) == 0 {
					t.Fatalf("%s lost its original definition", def.Function.Name)
				}
			}
			response, err := rt.Client.CompletionsWithCtx(context.Background(), llm.ChatRequest{
				Model: rt.Model, Messages: []llm.Message{llm.NewTextMessage("user", "Continue the OCR phase.")}, Tools: defs,
			})
			if err != nil {
				t.Fatalf("actual %s definitions rejected: %v", phase, err)
			}
			if response.VisibleContent() != "Ready for the next OCR phase" {
				t.Fatalf("unexpected response: %#v", response)
			}
			data, err := os.ReadFile(requestPath)
			if err != nil {
				t.Fatal(err)
			}
			var request llm.ChatRequest
			if err := json.Unmarshal(data, &request); err != nil {
				t.Fatal(err)
			}
			if len(request.Tools) != len(defs) {
				t.Fatalf("serialized %d tools, want %d", len(request.Tools), len(defs))
			}
			for i, def := range defs {
				if request.Tools[i].Function.Name != def.Function.Name {
					t.Fatalf("tool order/name lost at %d", i)
				}
				want, err := json.Marshal(def.Function.Parameters)
				if err != nil {
					t.Fatal(err)
				}
				got, err := json.Marshal(request.Tools[i].Function.Parameters)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != string(want) {
					t.Fatalf("tool %s lost parameter schema", def.Function.Name)
				}
			}
		})
	}
}
