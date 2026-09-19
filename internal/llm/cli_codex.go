// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"time"
)

// codexCLIClient uses the official app-server protocol, not an HTTP proxy.
// No-environment threads prevent native filesystem and command tools. Any
// native tool event is still rejected; only OCR may execute the returned calls.
type codexCLIClient struct{ cfg ClientConfig }

var codexCLIGate = make(chan struct{}, 1)

func (c *codexCLIClient) CompletionsWithCtx(ctx context.Context, req ChatRequest) (resp *ChatResponse, err error) {
	defer func() { finalizeRequest(ctx, c.cfg.retryCollector, err) }()
	if c.cfg.URL != "" || c.cfg.APIKey != "" || c.cfg.AuthHeader != "" || len(c.cfg.ExtraHeaders) > 0 || len(c.cfg.ExtraBody) > 0 || len(c.cfg.RetryCodes) > 0 || c.cfg.AWSProfile != "" || c.cfg.AWSRegion != "" {
		return nil, errors.New("Codex subscription transport rejects API and HTTP overrides")
	}
	if req.Temperature != nil {
		return nil, errors.New("Codex subscription transport does not support temperature")
	}
	if err = validateCLIRequest(req); err != nil {
		return nil, err
	}
	env, err := cliEnvironment()
	if err != nil {
		return nil, err
	}
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if value != "" && (strings.HasPrefix(key, "OPENAI_") || strings.HasPrefix(key, "CODEX_") || key == "AZURE_OPENAI_API_KEY") {
			return nil, errors.New("remove inherited Codex credential, endpoint, or runtime overrides")
		}
	}
	timeout := c.cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	select {
	case codexCLIGate <- struct{}{}:
		defer func() { <-codexCLIGate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	path, err := exec.LookPath("codex")
	if err != nil {
		return nil, errors.New("codex CLI executable not found")
	}
	cwd, err := os.MkdirTemp("", "ocr-codex-")
	if err != nil {
		return nil, errors.New("cannot create Codex working directory")
	}
	defer os.RemoveAll(cwd)
	version, err := runCLI(ctx, path, []string{"--version"}, env, cwd, nil)
	if err != nil {
		return nil, err
	}
	if !codexNoEnvironmentVersion(string(version)) {
		return nil, errors.New("Codex CLI 0.153.4 or newer is required for no-environment isolation")
	}

	if req.Model == "" {
		req.Model = c.cfg.Model
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, errors.New("cannot serialize Codex request")
	}
	cfg := codexIsolatedConfig()
	args := []string{"app-server", "--stdio", "--strict-config"}
	// Launch-time overrides disable customization before a thread can start.
	for _, key := range codexConfigKeys {
		value, _ := json.Marshal(cfg[key])
		args = append(args, "-c", key+"="+string(value))
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = cwd
	cmd.Env = env
	if err = configureCLIProcess(cmd); err != nil {
		return nil, err
	}
	cmd.WaitDelay = time.Second
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.New("cannot open Codex input")
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		return nil, errors.New("cannot open Codex output")
	}
	var stderr cliBoundedBuffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		return nil, errors.New("cannot start Codex app-server")
	}
	defer func() {
		input.Close()
		_ = cmd.Cancel()
		_ = cmd.Wait()
		if ctx.Err() != nil {
			resp = nil
			err = fmt.Errorf("Codex subscription request interrupted: %w", ctx.Err())
		}
		if stderr.overflow {
			resp = nil
			err = errors.New("Codex diagnostic output exceeded safety limit")
		}
	}()
	rpc := newCodexRPC(input, output)
	if _, err = rpc.call("initialize", map[string]any{"clientInfo": map[string]any{"name": "ocr-subscription", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}}); err != nil {
		return nil, err
	}
	if err = rpc.send(map[string]any{"method": "initialized"}); err != nil {
		return nil, err
	}
	names, err := rpc.configCheck(cwd, cfg)
	if err != nil {
		return nil, err
	}
	auth, err := rpc.call("account/read", map[string]any{"refreshToken": false})
	if err != nil {
		return nil, err
	}
	var account struct {
		Account *struct {
			Type string `json:"type"`
		} `json:"account"`
	}
	if json.Unmarshal(auth, &account) != nil || account.Account == nil || account.Account.Type != "chatgpt" {
		return nil, errors.New("Codex requires official ChatGPT subscription login")
	}
	// Disable servers by name before any inventory or model request.
	disabledServers := make(map[string]any, len(names))
	for _, name := range names {
		disabledServers[name] = map[string]any{"enabled": false}
	}
	// RPC config keys are not TOML expressions. Preserve each server name as
	// a literal nested map key, including names containing dots or quotes.
	cfg["mcp_servers"] = disabledServers
	var model any
	if req.Model != "" && req.Model != "default" {
		model = req.Model
	}
	started, err := rpc.call("thread/start", map[string]any{"model": model, "modelProvider": "openai", "ephemeral": true, "cwd": cwd, "environments": []any{}, "dynamicTools": []any{}, "approvalPolicy": "never", "sandbox": "read-only", "config": cfg, "baseInstructions": cliInstructions, "developerInstructions": "Do not call native tools. Return only the requested JSON assistant turn."})
	if err != nil {
		return nil, err
	}
	var thread struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
		ModelProvider string `json:"modelProvider"`
	}
	if json.Unmarshal(started, &thread) != nil || thread.Thread.ID == "" || thread.ModelProvider != "openai" {
		return nil, errors.New("Codex failed to confirm official isolated thread")
	}
	rpc.threadID = thread.Thread.ID
	// Disabled servers must not remain available in the thread runtime.
	remaining, err := rpc.mcpNames(rpc.threadID)
	if err != nil {
		return nil, err
	}
	if len(remaining) > 0 {
		return nil, errors.New("Codex isolated thread still exposes MCP servers")
	}
	var schema any
	if json.Unmarshal([]byte(cliResponseSchema), &schema) != nil {
		return nil, errors.New("invalid subscription response schema")
	}
	turnResult, err := rpc.call("turn/start", map[string]any{"threadId": rpc.threadID, "input": []any{map[string]any{"type": "text", "text": string(payload)}}, "environments": []any{}, "outputSchema": schema})
	if err != nil {
		return nil, err
	}
	var turn struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(turnResult, &turn) != nil || turn.Turn.ID == "" {
		return nil, errors.New("Codex did not start a turn")
	}
	rpc.turnID = turn.Turn.ID
	if rpc.observedTurn != "" && rpc.observedTurn != rpc.turnID {
		return nil, errors.New("Codex early notification turn mismatch")
	}
	for !rpc.done {
		msg, e := rpc.read()
		if e != nil {
			return nil, e
		}
		if e = rpc.notification(msg); e != nil {
			return nil, e
		}
	}
	if rpc.final == "" || rpc.usage == nil {
		return nil, errors.New("Codex response missing final output or valid token usage")
	}
	return parseCLIStructuredResponse([]byte(rpc.final), req, rpc.usage)
}

var codexConfigKeys = []string{"model_provider", "forced_login_method", "web_search", "features.shell_tool", "features.view_image", "features.multi_agent", "features.multi_agent_v2", "features.plugins", "features.apps", "features.hooks", "features.codex_hooks", "features.plugin_hooks", "features.code_mode", "features.code_mode_only", "features.js_repl", "features.skip_host_skill_discovery", "features.token_budget", "features.deferred_executor", "features.current_time_reminder", "features.sleep_tool", "features.tool_suggest", "features.image_generation", "features.browser_use", "features.computer_use", "tools.update_plan.enabled", "tools.experimental_request_user_input.enabled"}

func codexIsolatedConfig() map[string]any {
	cfg := map[string]any{}
	for _, k := range codexConfigKeys {
		cfg[k] = false
	}
	cfg["model_provider"] = "openai"
	cfg["forced_login_method"] = "chatgpt"
	cfg["web_search"] = "disabled"
	cfg["features.skip_host_skill_discovery"] = true
	return cfg
}

type codexRPCMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}
type codexRPC struct {
	writer                                io.Writer
	scanner                               *bufio.Scanner
	next                                  int
	bytes                                 int
	threadID, turnID, observedTurn, final string
	usage                                 *UsageInfo
	done                                  bool
}

func newCodexRPC(w io.Writer, r io.Reader) *codexRPC {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), cliOutputLimit)
	return &codexRPC{writer: w, scanner: s}
}
func (r *codexRPC) send(v any) error {
	if json.NewEncoder(r.writer).Encode(v) != nil {
		return errors.New("cannot write Codex RPC")
	}
	return nil
}
func (r *codexRPC) read() (codexRPCMessage, error) {
	var m codexRPCMessage
	if !r.scanner.Scan() {
		return m, errors.New("Codex RPC stream ended or exceeded safety limit")
	}
	b := r.scanner.Bytes()
	r.bytes += len(b)
	if r.bytes > cliOutputLimit {
		return m, errors.New("Codex RPC output exceeded safety limit")
	}
	if json.Unmarshal(b, &m) != nil {
		return m, errors.New("invalid Codex RPC output")
	}
	return m, nil
}
func (r *codexRPC) call(method string, params any) (json.RawMessage, error) {
	r.next++
	id := r.next
	if err := r.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		m, err := r.read()
		if err != nil {
			return nil, err
		}
		if m.Method != "" {
			if err = r.notification(m); err != nil {
				return nil, err
			}
			continue
		}
		var got int
		if json.Unmarshal(m.ID, &got) != nil || got != id {
			return nil, errors.New("unexpected Codex RPC response")
		}
		if len(m.Error) > 0 && string(m.Error) != "null" {
			return nil, fmt.Errorf("Codex RPC %s failed (diagnostics withheld)", method)
		}
		if len(m.Result) == 0 {
			return nil, errors.New("missing Codex RPC result")
		}
		return m.Result, nil
	}
}
func (r *codexRPC) mcpNames(thread string) ([]string, error) {
	var names []string
	cursor := ""
	seen := map[string]bool{}
	for {
		params := map[string]any{"limit": 100, "detail": "toolsAndAuthOnly"}
		if thread != "" {
			params["threadId"] = thread
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		result, err := r.call("mcpServerStatus/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Data *[]struct {
				Name          string                     `json:"name"`
				RuntimeStatus string                     `json:"runtimeStatus"`
				Tools         map[string]json.RawMessage `json:"tools"`
				Resources     []json.RawMessage          `json:"resources"`
				Templates     []json.RawMessage          `json:"resourceTemplates"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if json.Unmarshal(result, &page) != nil || page.Data == nil {
			return nil, errors.New("invalid Codex MCP status")
		}
		for _, server := range *page.Data {
			if server.Name == "" {
				return nil, errors.New("invalid Codex MCP name")
			}
			// The official status API includes disabled server metadata.
			// A disabled server is safe only if it exposes no capabilities.
			if thread != "" && server.RuntimeStatus == "disabled" && server.Tools != nil && server.Resources != nil && server.Templates != nil && len(server.Tools) == 0 && len(server.Resources) == 0 && len(server.Templates) == 0 {
				continue
			}
			names = append(names, server.Name)
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			return names, nil
		}
		cursor = *page.NextCursor
		if seen[cursor] {
			return nil, errors.New("repeated Codex MCP pagination cursor")
		}
		seen[cursor] = true
	}
}
func (r *codexRPC) notification(m codexRPCMessage) error {
	if len(m.ID) > 0 && string(m.ID) != "null" {
		_ = r.send(map[string]any{"id": m.ID, "error": map[string]any{"code": -32601, "message": "OCR rejects native tool and approval requests"}})
		return errors.New("Codex requested an unauthorized native operation")
	}
	var p struct {
		ThreadID string          `json:"threadId"`
		TurnID   string          `json:"turnId"`
		Error    json.RawMessage `json:"error"`
		Item     struct {
			Type  string `json:"type"`
			Text  string `json:"text"`
			Phase string `json:"phase"`
		} `json:"item"`
		Turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
		TokenUsage struct {
			Total codexUsage `json:"total"`
		} `json:"tokenUsage"`
	}
	if len(m.Params) > 0 && json.Unmarshal(m.Params, &p) != nil {
		return errors.New("invalid Codex notification")
	}
	if p.ThreadID != "" && r.threadID != "" && p.ThreadID != r.threadID {
		return errors.New("Codex notification thread mismatch")
	}
	if p.TurnID != "" {
		if r.observedTurn != "" && r.observedTurn != p.TurnID {
			return errors.New("Codex notification turn mismatch")
		}
		r.observedTurn = p.TurnID
	}
	if p.TurnID != "" && r.turnID != "" && p.TurnID != r.turnID {
		return errors.New("Codex notification turn mismatch")
	}
	switch m.Method {
	case "item/started", "item/completed":
		switch p.Item.Type {
		case "userMessage", "reasoning":
		case "agentMessage":
			if m.Method == "item/completed" && (p.Item.Phase == "final_answer" || p.Item.Phase == "") {
				r.final = p.Item.Text
			}
		default:
			return errors.New("Codex used a native tool instead of returning OCR tool data")
		}
	case "thread/tokenUsage/updated":
		u, err := p.TokenUsage.Total.convert()
		if err != nil {
			return err
		}
		r.usage = u
	case "turn/completed":
		if p.Turn.ID == "" || (r.observedTurn != "" && r.observedTurn != p.Turn.ID) {
			return errors.New("Codex completion turn mismatch")
		}
		r.observedTurn = p.Turn.ID
		if p.Turn.Status != "completed" {
			return errors.New("Codex turn failed or was interrupted")
		}
		if r.turnID != "" && p.Turn.ID != r.turnID {
			return errors.New("Codex completion turn mismatch")
		}
		r.done = true
	case "error":
		return fmt.Errorf("Codex reported a request error (%s)", codexSafeError(p.Error))
	case "thread/started", "thread/status/changed", "thread/name/updated", "turn/started", "account/updated", "account/rateLimits/updated", "remoteControl/status/changed", "deprecationNotice", "model/verification", "model/safetyBuffering/updated", "turn/moderationMetadata", "warning", "configWarning", "item/agentMessage/delta", "item/reasoning/summaryTextDelta", "item/reasoning/textDelta", "item/reasoning/summaryPartAdded":
	default:
		return fmt.Errorf("unexpected Codex notification: %s", codexSafeMethod(m.Method))
	}
	return nil
}

type codexUsage struct {
	Total  *int64 `json:"totalTokens"`
	Input  *int64 `json:"inputTokens"`
	Output *int64 `json:"outputTokens"`
	Cached *int64 `json:"cachedInputTokens"`
	Write  int64  `json:"cacheWriteInputTokens"`
}

func (u codexUsage) convert() (*UsageInfo, error) {
	if u.Total == nil || u.Input == nil || u.Output == nil || u.Cached == nil || *u.Total < 0 || *u.Input < 0 || *u.Output < 0 || *u.Cached < 0 || u.Write < 0 || *u.Cached > *u.Input || *u.Input > *u.Total || *u.Output > *u.Total-*u.Input {
		return nil, errors.New("Codex response missing valid token usage")
	}
	return &UsageInfo{TotalTokens: *u.Total, PromptTokens: *u.Input, CompletionTokens: *u.Output, CacheReadTokens: *u.Cached, CacheWriteTokens: u.Write}, nil
}

// config/read is a non-executing official RPC. Inspect only the effective
// configuration in memory; never log it or load local credentials ourselves.
func (r *codexRPC) configCheck(cwd string, expected map[string]any) ([]string, error) {
	raw, err := r.call("config/read", map[string]any{"includeLayers": true, "cwd": cwd})
	if err != nil {
		return nil, err
	}
	var result struct {
		Config  map[string]json.RawMessage `json:"config"`
		Origins map[string]struct {
			Name struct {
				Type string `json:"type"`
			} `json:"name"`
		} `json:"origins"`
		Layers []struct {
			Name struct {
				Type string `json:"type"`
			} `json:"name"`
			Config map[string]any `json:"config"`
		} `json:"layers"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Config == nil {
		return nil, errors.New("Codex effective configuration unavailable")
	}
	if v := result.Config["chatgpt_base_url"]; len(v) > 0 && string(v) != "null" {
		var url string
		if json.Unmarshal(v, &url) != nil || (url != "" && url != "https://chatgpt.com/backend-api" && url != "https://chatgpt.com/backend-api/") {
			return nil, errors.New("Codex subscription rejects chatgpt_base_url override")
		}
	}
	if v := result.Config["model_catalog_json"]; len(v) > 0 && string(v) != "null" && string(v) != `""` {
		return nil, errors.New("Codex subscription rejects model_catalog_json override")
	}
	var providers map[string]map[string]json.RawMessage
	if v := result.Config["model_providers"]; len(v) > 0 && string(v) != "null" {
		if json.Unmarshal(v, &providers) != nil {
			return nil, errors.New("invalid Codex provider configuration")
		}
		if len(providers["openai"]) > 0 {
			return nil, errors.New("Codex subscription rejects custom official-provider overrides")
		}
	}
	// CLI overrides must be reflected in the effective configuration, not
	// silently discarded by a version or organization policy.
	var generic map[string]any
	if json.Unmarshal(raw, &generic) != nil {
		return nil, errors.New("invalid Codex configuration")
	}
	tree, _ := generic["config"].(map[string]any)
	for key, want := range expected {
		var source map[string]any = tree
		// ToolsV2 projects only web_search in Codex 0.153.4. The official
		// sessionFlags layer preserves these two validated CLI overrides.
		if strings.HasPrefix(key, "tools.") {
			if result.Origins[key].Name.Type != "sessionFlags" {
				return nil, errors.New("Codex tool isolation override is not authoritative")
			}
			source = nil
			for _, layer := range result.Layers {
				if layer.Name.Type == "sessionFlags" {
					source = layer.Config
				}
			}
		}
		var got any = source
		for _, part := range strings.Split(key, ".") {
			m, ok := got.(map[string]any)
			if !ok {
				got = nil
				break
			}
			got = m[part]
		}
		if !reflect.DeepEqual(got, want) {
			return nil, errors.New("Codex isolation configuration was not applied")
		}
	}
	var servers map[string]json.RawMessage
	if v := result.Config["mcp_servers"]; len(v) > 0 && string(v) != "null" {
		if json.Unmarshal(v, &servers) != nil {
			return nil, errors.New("invalid Codex MCP configuration")
		}
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		if name == "" {
			return nil, errors.New("invalid Codex MCP name")
		}
		names = append(names, name)
	}
	return names, nil
}

func codexNoEnvironmentVersion(text string) bool {
	var major, minor, patch int
	n, err := fmt.Sscanf(strings.TrimSpace(text), "codex-cli %d.%d.%d", &major, &minor, &patch)
	return err == nil && n == 3 && (major > 0 || (major == 0 && (minor > 153 || (minor == 153 && patch >= 4))))
}

// Report only protocol-shaped method identifiers, never arbitrary server text
// or params, so compatibility errors are diagnosable without leaking secrets.
func codexSafeMethod(method string) string {
	if len(method) == 0 || len(method) > 128 {
		return "[invalid-method]"
	}
	for _, c := range method {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '/' || c == '_' || c == '-' || c == '.') {
			return "[invalid-method]"
		}
	}
	return method
}

// Report only enumerated error categories. Error messages may contain provider
// payloads, so parse known machine codes rather than forwarding arbitrary text.
func codexSafeError(raw json.RawMessage) string {
	var detail struct {
		Info    json.RawMessage `json:"codexErrorInfo"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(raw, &detail) != nil {
		return "unknown"
	}
	var provider struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(detail.Message), &provider) == nil {
		switch provider.Error.Code {
		case "invalid_json_schema", "model_not_found", "rate_limit_exceeded", "insufficient_quota", "invalid_api_key", "context_length_exceeded":
			return provider.Error.Code
		}
	}
	var kind string
	if json.Unmarshal(detail.Info, &kind) != nil {
		var tagged map[string]json.RawMessage
		if json.Unmarshal(detail.Info, &tagged) == nil && len(tagged) == 1 {
			for key := range tagged {
				kind = key
			}
		}
	}
	switch kind {
	case "contextWindowExceeded", "usageLimitExceeded", "httpConnectionFailed", "responseStreamConnectionFailed", "responseStreamDisconnected", "responseTooManyFailedAttempts", "badRequest", "unauthorized", "serverOverloaded", "internalServerError", "sandboxError", "other":
		return kind
	}
	return "unknown"
}
