// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"github.com/alibaba/open-code-review/internal/llm"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIConfigSetAndResolve(t *testing.T) {
	for _, protocol := range []string{llm.ProtocolClaudeCLI, llm.ProtocolCodexCLI} {
		cfg := &Config{Provider: "openai", Model: "old-model"}
		if err := setConfigValue(cfg, "provider", protocol); err != nil {
			t.Fatal(err)
		}
		if cfg.Model != "" {
			t.Fatal("old model retained")
		}
		if err := setConfigValue(cfg, "model", "my-model"); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "config.json")
		if err := saveConfig(path, cfg); err != nil {
			t.Fatal(err)
		}
		ep, err := llm.ResolveEndpoint(path)
		if err != nil || ep.Protocol != protocol || ep.Model != "my-model" {
			t.Fatalf("bad resolution: %+v %v", ep, err)
		}
	}
}

func TestCLIPresetSkipsKeyPrompt(t *testing.T) {
	for _, name := range []string{llm.ProtocolClaudeCLI, llm.ProtocolCodexCLI} {
		p, _ := llm.LookupProvider(name)
		if err := checkAPIKeyRequirement(name, "", "", p, true); err != nil {
			t.Fatal(err)
		}
		m := newProviderTUI(&Config{}, "")
		for i, p := range m.providers {
			if p.Name == name {
				m.officialIdx = i
			}
		}
		m.step = stepModel
		next, _ := m.Update(enterKey())
		result := next.(providerTUIModel)
		if !result.confirmed || result.step == stepAPIKey {
			t.Fatal("CLI requested API key")
		}
		if strings.Contains(result.View().Content, "AWS") {
			t.Fatal("CLI shown as AWS")
		}
	}
}
