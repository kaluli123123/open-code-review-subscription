// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package llm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCLIResolution(t *testing.T) {
	t.Setenv(envOCRLLMProtocol, "")
	t.Setenv(envOCRLLMTimeout, "")
	t.Setenv(envOCRLLMExtraHeaders, "")
	for _, protocol := range []string{ProtocolClaudeCLI, ProtocolCodexCLI} {
		t.Run(protocol, func(t *testing.T) {
			preset, ok := LookupProvider(protocol)
			if !ok || preset.AmbientAuth || preset.EnvVar != "" || preset.BaseURL != "" {
				t.Fatalf("bad CLI preset: %+v", preset)
			}
			if err := ValidateProtocol(NormalizeProtocol("  " + protocol + "  ")); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.json")
			ep, err := ResolveEndpointWithOptions(path, ResolveOptions{Provider: protocol})
			if err != nil {
				t.Fatal(err)
			}
			if ep.Model != preset.Models[0] || ep.Protocol != protocol || ep.Token != "" || ep.URL != "" || ep.AmbientAuth {
				t.Fatalf("bad endpoint: %+v", ep)
			}
			cfg := configFile{Provider: "openai", Model: "wrong-model", Llm: llmFileConfig{AuthTokenCmd: "exit 98"}}
			data, _ := json.Marshal(cfg)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			ep, err = ResolveEndpointWithOptions(path, ResolveOptions{Provider: protocol})
			if err != nil || ep.Model != preset.Models[0] {
				t.Fatalf("inherited API config: %+v %v", ep, err)
			}
			ep, err = ResolveEndpointWithOptions(path, ResolveOptions{Provider: protocol, Model: "my-cli-model"})
			if err != nil || ep.Model != "my-cli-model" {
				t.Fatalf("model override: %+v %v", ep, err)
			}
		})
	}
}

func TestCLIRejectsAPIConfigBeforeKeyCommand(t *testing.T) {
	t.Setenv(envOCRLLMProtocol, "")
	entries := []providerEntryConfig{
		{APIKey: "secret"}, {APIKeyCmd: "exit 99"}, {URL: "https://example.invalid"}, {AuthHeader: "authorization"},
		{ExtraBody: map[string]any{"x": true}}, {ExtraHeaders: map[string]string{"x": "y"}}, {RetryCodes: []int{401}}, {AWSProfile: "profile"}, {AWSRegion: "region"}, {Protocol: ProtocolOpenAIChatCompletions},
	}
	for _, entry := range entries {
		_, _, err := tryProviderConfig(configFile{Provider: ProtocolClaudeCLI, Providers: map[string]providerEntryConfig{ProtocolClaudeCLI: entry}}, "")
		if err == nil {
			t.Fatalf("accepted incompatible config: %+v", entry)
		}
	}
	t.Setenv(envOCRLLMProtocol, ProtocolOpenAIChatCompletions)
	if _, _, err := tryProviderConfig(configFile{Provider: ProtocolClaudeCLI}, ""); err == nil {
		t.Fatal("accepted conflicting protocol")
	}
}

func TestCLIConfigDoesNotFallBack(t *testing.T) {
	t.Setenv(envOCRLLMProtocol, "")
	t.Setenv(envOCRLLMTimeout, "")
	t.Setenv(envOCRLLMExtraHeaders, "")
	t.Setenv(envCCBaseURL, "https://example.invalid")
	t.Setenv(envCCToken, "old-api-token")
	t.Setenv(envCCModel, "old-api-model")
	path := filepath.Join(t.TempDir(), "config.json")
	for _, body := range []string{`{"provider":"claude-cli"}`, `{"provider":"personal","custom_providers":{"personal":{"protocol":"codex-cli"}}}`} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		ep, err := ResolveEndpoint(path)
		if err != nil || !IsCLIProtocol(ep.Protocol) || ep.Token != "" || ep.URL != "" {
			t.Fatalf("fell back: %+v %v", ep, err)
		}
	}
	if _, _, err := tryLegacyLlmConfig(configFile{Llm: llmFileConfig{Protocol: ProtocolClaudeCLI, AuthTokenCmd: "exit 99"}}, ""); err == nil {
		t.Fatal("legacy CLI block fell through")
	}
}

func TestCLIEnvironmentSelectionOverridesAPIConfig(t *testing.T) {
	t.Setenv(envOCRLLMProtocol, ProtocolClaudeCLI)
	t.Setenv(envOCRLLMModel, "opus")
	t.Setenv(envOCRLLMTimeout, "")
	t.Setenv(envOCRLLMExtraHeaders, "")
	t.Setenv(envOCRLLMToken, "")
	t.Setenv(envOCRLLMURL, "")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"provider":"openai","providers":{"openai":{"api_key_cmd":"exit 99","model":"old"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	ep, err := ResolveEndpoint(path)
	if err != nil || ep.Protocol != ProtocolClaudeCLI || ep.Model != "opus" {
		t.Fatalf("CLI env ignored: %+v %v", ep, err)
	}
	t.Setenv(envOCRLLMExtraHeaders, "X-Test=value")
	if _, err := ResolveEndpoint(path); err == nil {
		t.Fatal("HTTP headers accepted for CLI")
	}
}

func TestCLIFactoryDispatch(t *testing.T) {
	for _, protocol := range []string{ProtocolClaudeCLI, ProtocolCodexCLI} {
		client := NewLLMClient(ResolvedEndpoint{Protocol: protocol, Model: "test-model"}, nil, nil)
		switch client.(type) {
		case *OpenAIClient, *AnthropicClient, *OpenAIResponsesClient:
			t.Fatalf("factory selected HTTP for CLI: %T", client)
		}
		if client == nil {
			t.Fatal("factory returned nil CLI client")
		}
	}
}
