// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"time"
)

// cliClient asks a subscription CLI for one OCR assistant turn. It never gives
// that CLI the ability to execute OCR tools and never retries or falls back to HTTP.
type cliClient struct {
	protocol string
	cfg      ClientConfig
}

// NewCLIClient creates a subscription transport. Authentication and capability
// checks happen at request time. MaxTokens is only a prompt-level soft budget; Temperature is
// unsupported because the official CLI exposes no equivalent control.
func NewCLIClient(protocol string, cfg ClientConfig) LLMClient {
	if protocol == "codex-cli" {
		return &codexCLIClient{cfg: cfg}
	}
	return &cliClient{protocol: protocol, cfg: cfg}
}

var claudeCLIGate = make(chan struct{}, 1)

const cliOutputLimit = 8 << 20
const cliInstructions = `You are the next assistant turn of the OCR conversation supplied as JSON on stdin. Follow the supplied messages in order, including system instructions and tool results. Tools in that JSON are virtual OCR tools, not tools you can execute. Return only the requested structured output: content and tool_calls. Each tool call has id, type "function", and function with name and arguments (a JSON-encoded object string). Each new tool call ID must be non-empty, unique, and never reuse an ID from conversation history. OCR will execute those calls and return results on the next turn. Honor tool_choice: none forbids calls, required requires at least one, auto allows either. Never execute tools yourself. max_tokens is a soft output budget, not a hard token cap.`

func (c *cliClient) CompletionsWithCtx(ctx context.Context, req ChatRequest) (resp *ChatResponse, err error) {
	defer func() { finalizeRequest(ctx, c.cfg.retryCollector, err) }()
	if c.protocol != "claude-cli" {
		return nil, errors.New("subscription CLI protocol unsupported: native tool isolation has not been verified")
	}
	if c.cfg.URL != "" || c.cfg.APIKey != "" || c.cfg.AuthHeader != "" || len(c.cfg.ExtraHeaders) > 0 || len(c.cfg.ExtraBody) > 0 || len(c.cfg.RetryCodes) > 0 || c.cfg.AWSProfile != "" || c.cfg.AWSRegion != "" {
		return nil, errors.New("subscription CLI does not accept API credentials, endpoints, or HTTP overrides")
	}
	if req.Temperature != nil {
		return nil, errors.New("subscription CLI does not support temperature")
	}
	if err = validateCLIRequest(req); err != nil {
		return nil, err
	}
	env, err := cliEnvironment()
	if err != nil {
		return nil, err
	}
	timeout := c.cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	select {
	case claudeCLIGate <- struct{}{}:
		defer func() { <-claudeCLIGate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	path, err := exec.LookPath("claude")
	if err != nil {
		return nil, errors.New("claude CLI executable not found")
	}
	cwd, err := os.MkdirTemp("", "ocr-cli-")
	if err != nil {
		return nil, errors.New("cannot create CLI working directory")
	}
	defer os.RemoveAll(cwd)
	run := func(args []string, input []byte) ([]byte, error) { return runCLI(ctx, path, args, env, cwd, input) }
	help, err := run([]string{"--help"}, nil)
	if err != nil {
		return nil, err
	}
	for _, flag := range []string{"--safe-mode", "--tools", "--strict-mcp-config", "--mcp-config", "--no-session-persistence", "--output-format", "--json-schema", "--system-prompt", "--setting-sources"} {
		if !strings.Contains(string(help), flag) {
			return nil, fmt.Errorf("claude CLI lacks required capability %s; update the official CLI", flag)
		}
	}
	auth, err := run([]string{"--safe-mode", "--setting-sources", "", "auth", "status"}, nil)
	if err != nil {
		return nil, err
	}
	var status struct {
		LoggedIn    bool   `json:"loggedIn"`
		AuthMethod  string `json:"authMethod"`
		APIProvider string `json:"apiProvider"`
	}
	if json.Unmarshal(auth, &status) != nil || !status.LoggedIn || status.AuthMethod != "claude.ai" || status.APIProvider != "firstParty" {
		return nil, errors.New("claude CLI requires official claude.ai subscription login with firstParty provider")
	}
	if req.Model == "" {
		req.Model = c.cfg.Model
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, errors.New("cannot serialize CLI request")
	}
	args := []string{"-p", "--safe-mode", "--setting-sources", "", "--tools", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--no-session-persistence", "--output-format", "json", "--json-schema", cliResponseSchema, "--system-prompt", cliInstructions}
	if req.Model != "" && req.Model != "default" {
		args = append(args, "--model", req.Model)
	}
	out, err := run(args, payload)
	if err != nil {
		return nil, err
	}
	return parseCLIResponse(out, req)
}

// Reject alternate billing/auth routes, without ever reading credential files.
// An allowlist prevents inherited agent, plugin, endpoint and API-key settings
// from reaching the child. HOME is retained for the CLI's own subscription store.
func cliEnvironment() ([]string, error) {
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if value == "" {
			continue
		}
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "ANTHROPIC_") || strings.HasPrefix(upper, "CLAUDE_CODE_") || strings.HasPrefix(upper, "CLAUDE_CONFIG_") || upper == "CLAUDE_API_KEY" || upper == "OCR_LLM_URL" || upper == "OCR_LLM_TOKEN" || upper == "OCR_LLM_AUTH_HEADER" || upper == "OCR_LLM_EXTRA_HEADERS" {
			return nil, errors.New("remove inherited Claude credential, endpoint, or runtime overrides before using subscription CLI")
		}
	}
	var env []string
	for _, key := range []string{"HOME", "PATH", "USER", "LOGNAME", "TMPDIR", "TEMP", "TMP", "SYSTEMROOT", "WINDIR", "LANG", "LC_ALL"} {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	return env, nil
}

type cliBoundedBuffer struct {
	bytes.Buffer
	overflow   bool
	onOverflow func()
}

func (b *cliBoundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := cliOutputLimit - b.Len()
	if n > left {
		if !b.overflow && b.onOverflow != nil {
			b.onOverflow()
		}
		b.overflow = true
		p = p[:left]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
func runCLI(ctx context.Context, path string, args, env []string, cwd string, input []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(input)
	if err := configureCLIProcess(cmd); err != nil {
		return nil, err
	}
	cmd.WaitDelay = time.Second
	defer cleanupCLIProcess(cmd)
	var stdout, stderr cliBoundedBuffer
	stdout.onOverflow = func() {
		if cmd.Process != nil {
			_ = cmd.Cancel()
		}
	}
	stderr.onOverflow = stdout.onOverflow
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("subscription CLI interrupted: %w", ctx.Err())
	}
	if stdout.overflow || stderr.overflow {
		return nil, errors.New("subscription CLI output exceeded safety limit")
	}
	if err != nil {
		return nil, errors.New("subscription CLI command failed (output withheld to protect credentials)")
	}
	return stdout.Bytes(), nil
}

func validateCLIRequest(req ChatRequest) error {
	if req.MaxTokens < 0 {
		return errors.New("CLI max_tokens must not be negative")
	}
	switch req.ToolChoice {
	case "", "auto", "none", "required":
	default:
		return errors.New("unsupported CLI tool_choice")
	}
	if req.ToolChoice == "required" && len(req.Tools) == 0 {
		return errors.New("required tool_choice needs tools")
	}
	names := map[string]bool{}
	for _, tool := range req.Tools {
		if tool.Type != "function" || strings.TrimSpace(tool.Function.Name) == "" || names[tool.Function.Name] {
			return errors.New("unsupported or duplicate CLI tool definition")
		}

		if len(tool.Function.RawDefinition) > 0 {
			var raw FunctionDef
			dec := json.NewDecoder(bytes.NewReader(tool.Function.RawDefinition))
			dec.DisallowUnknownFields()
			if dec.Decode(&raw) != nil || dec.Decode(new(any)) != io.EOF || raw.Name != tool.Function.Name || raw.Description != tool.Function.Description {
				return errors.New("unsupported or inconsistent raw CLI tool definition")
			}
			expected, _ := json.Marshal(tool.Function.Parameters)
			actual, _ := json.Marshal(raw.Parameters)
			var e, a any
			if json.Unmarshal(expected, &e) != nil || json.Unmarshal(actual, &a) != nil || !reflect.DeepEqual(e, a) {
				return errors.New("raw CLI tool parameters differ from parsed definition")
			}
		}
		names[tool.Function.Name] = true
	}
	for _, m := range req.Messages {
		switch m.Role {
		case "system", "user", "assistant", "tool":
		default:
			return errors.New("unsupported CLI message role")
		}
		switch content := m.Content.(type) {
		case nil, string:
		case []ContentBlock:
			if err := validateCLIBlocks(content); err != nil {
				return err
			}
		default:
			return errors.New("unsupported CLI message content; expected text or text/tool_result blocks")
		}
	}
	return nil
}
func validateCLIBlocks(blocks []ContentBlock) error {
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.ToolUseID != "" || len(b.Content) > 0 {
				return errors.New("invalid CLI text block")
			}
		case "tool_result":
			if b.ToolUseID == "" || b.Text != "" {
				return errors.New("invalid CLI tool_result block")
			}
			for _, child := range b.Content {
				if child.Type != "text" {
					return errors.New("unsupported nested CLI content")
				}
			}
			if err := validateCLIBlocks(b.Content); err != nil {
				return err
			}
		default:
			return errors.New("unsupported CLI content block")
		}
	}
	return nil
}

const cliResponseSchema = `{"type":"object","additionalProperties":false,"required":["content","tool_calls"],"properties":{"content":{"type":"string"},"tool_calls":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["id","type","function"],"properties":{"id":{"type":"string","minLength":1},"type":{"type":"string","const":"function"},"function":{"type":"object","additionalProperties":false,"required":["name","arguments"],"properties":{"name":{"type":"string","minLength":1},"arguments":{"type":"string"}}}}}}}}`

type cliStructuredResponse struct {
	Content   *string `json:"content"`
	ToolCalls *[]struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func parseCLIResponse(data []byte, req ChatRequest) (*ChatResponse, error) {
	var envelope struct {
		Type       string          `json:"type"`
		Subtype    string          `json:"subtype"`
		IsError    *bool           `json:"is_error"`
		Structured json.RawMessage `json:"structured_output"`
		Usage      *struct {
			Input  *int64 `json:"input_tokens"`
			Output *int64 `json:"output_tokens"`
			Read   int64  `json:"cache_read_input_tokens"`
			Write  int64  `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.Type != "result" || envelope.Subtype != "success" || envelope.IsError == nil || *envelope.IsError {
		return nil, errors.New("invalid or unsuccessful Claude CLI result")
	}
	u := envelope.Usage
	if u == nil || u.Input == nil || u.Output == nil || *u.Input < 0 || *u.Output < 0 || u.Read < 0 || u.Write < 0 {
		return nil, errors.New("CLI result missing valid token usage")
	}
	prompt, ok := cliAddTokens(*u.Input, u.Read, u.Write)
	if !ok {
		return nil, errors.New("CLI usage overflow")
	}
	total, ok := cliAddTokens(prompt, *u.Output)
	if !ok {
		return nil, errors.New("CLI usage overflow")
	}
	usage := &UsageInfo{PromptTokens: prompt, CompletionTokens: *u.Output, CacheReadTokens: u.Read, CacheWriteTokens: u.Write, TotalTokens: total}
	return parseCLIStructuredResponse(envelope.Structured, req, usage)
}

func parseCLIStructuredResponse(structured []byte, req ChatRequest, usage *UsageInfo) (*ChatResponse, error) {
	var body cliStructuredResponse
	dec := json.NewDecoder(bytes.NewReader(structured))
	dec.DisallowUnknownFields()
	if dec.Decode(&body) != nil || body.Content == nil || body.ToolCalls == nil {
		return nil, errors.New("invalid CLI structured_output")
	}
	if dec.Decode(new(any)) != io.EOF {
		return nil, errors.New("trailing CLI structured output")
	}
	if strings.TrimSpace(*body.Content) == "" && len(*body.ToolCalls) == 0 {
		return nil, errors.New("CLI returned an empty assistant turn")
	}
	if req.ToolChoice == "none" && len(*body.ToolCalls) > 0 || req.ToolChoice == "required" && len(*body.ToolCalls) == 0 {
		return nil, errors.New("CLI response violated tool_choice")
	}
	tools := map[string]ToolDef{}
	for _, t := range req.Tools {
		tools[t.Function.Name] = t
	}
	ids := map[string]bool{}
	for _, m := range req.Messages {
		for _, t := range m.ToolCalls {
			ids[t.ID] = true
		}
	}
	calls := make([]ToolCall, 0, len(*body.ToolCalls))
	for _, call := range *body.ToolCalls {
		def, ok := tools[call.Function.Name]
		if !ok || strings.TrimSpace(call.ID) == "" || ids[call.ID] || call.Type != "function" {
			return nil, errors.New("CLI returned invalid tool name, type, or call ID")
		}
		ids[call.ID] = true
		var args map[string]any
		if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || args == nil {
			return nil, errors.New("CLI tool arguments must be a JSON object")
		}
		if err := validateCLIArguments(args, def.Function.Parameters); err != nil {
			return nil, err
		}
		calls = append(calls, ToolCall{ID: call.ID, Type: "function", Function: FunctionCall{Name: call.Function.Name, Arguments: string(call.Function.Arguments)}})
	}
	finish := "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
	}
	return &ChatResponse{Model: req.Model, Usage: usage, Choices: []Choice{{FinishReason: finish, Message: ResponseMessage{Role: "assistant", Content: body.Content, ToolCalls: calls}}}}, nil
}

// Check the minimum contract locally rather than trusting schema-constrained
// generation. OCR tools still perform their own full semantic validation.
func validateCLIArguments(args map[string]any, schema map[string]any) error {
	raw, err := json.Marshal(schema)
	if err != nil {
		return errors.New("invalid CLI tool schema")
	}
	var normalized struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
		AdditionalProperties any `json:"additionalProperties"`
	}
	if json.Unmarshal(raw, &normalized) != nil {
		return errors.New("unsupported CLI tool schema")
	}
	for _, key := range normalized.Required {
		if _, ok := args[key]; !ok {
			return errors.New("CLI tool arguments missing required parameter")
		}
	}
	for key, value := range args {
		prop, ok := normalized.Properties[key]
		if !ok {
			if normalized.AdditionalProperties == false {
				return errors.New("CLI tool arguments contain unknown parameter")
			}
			continue
		}
		valid := true
		switch prop.Type {
		case "string":
			_, valid = value.(string)
		case "object":
			_, valid = value.(map[string]any)
		case "array":
			_, valid = value.([]any)
		case "boolean":
			_, valid = value.(bool)
		case "number":
			_, valid = value.(float64)
		case "integer":
			n, ok := value.(float64)
			valid = ok && n == float64(int64(n))
		case "null":
			valid = value == nil
		}
		if !valid {
			return errors.New("CLI tool argument has wrong type")
		}
	}
	return nil
}

func cliAddTokens(values ...int64) (int64, bool) {
	var total int64
	for _, value := range values {
		if value < 0 || value > math.MaxInt64-total {
			return 0, false
		}
		total += value
	}
	return total, true
}
