package config_test

import (
	"testing"

	"switchai/config"
)

func TestProviderIsCopilot(t *testing.T) {
	tests := []struct {
		name              string
		copilotBaseURL    string
		expectedIsCopilot bool
	}{
		{"empty base URL means not Copilot", "", false},
		{"github.com domain is Copilot", "github.com", true},
		{"GHES domain is Copilot", "copilot.example.com", true},
		{"copilot-api subdomain is Copilot", "copilot-api.company.com", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := config.Provider{
				ID:             "test",
				CopilotBaseURL: tc.copilotBaseURL,
			}
			if p.IsCopilot() != tc.expectedIsCopilot {
				t.Errorf("IsCopilot() = %v, want %v", p.IsCopilot(), tc.expectedIsCopilot)
			}
		})
	}
}

func TestResolveModel(t *testing.T) {
	p := config.Provider{
		DefaultModel: "claude-sonnet-4-6",
		HaikuModel:   "claude-haiku-4-5",
		SonnetModel:  "claude-sonnet-4-6",
		OpusModel:    "claude-opus-4-7",
		FastModel:    "claude-haiku-4-5",
	}

	tests := []struct {
		input    string
		expected string
	}{
		{"default_model", "claude-sonnet-4-6"},
		{"haiku_model", "claude-haiku-4-5"},
		{"sonnet_model", "claude-sonnet-4-6"},
		{"opus_model", "claude-opus-4-7"},
		{"fast_model", "claude-haiku-4-5"},
		{"unknown_key", "claude-sonnet-4-6"}, // falls back to default_model
		{"", "claude-sonnet-4-6"},             // falls back to default_model
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result := p.ResolveModel(tc.input)
			if result != tc.expected {
				t.Errorf("ResolveModel(%q) = %q, want %q", tc.input, result, tc.expected)
			}
		})
	}
}

func TestResolveModelFallbackToModelField(t *testing.T) {
	p := config.Provider{
		Model:        "claude-3-sonnet",
		DefaultModel: "",
	}

	result := p.ResolveModel("default_model")
	if result != "claude-3-sonnet" {
		t.Errorf("ResolveModel = %q, want %q", result, "claude-3-sonnet")
	}
}
