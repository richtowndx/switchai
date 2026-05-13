package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"switchai/config"
)

// ============================================================
// NormalizeCopilotModelID 测试
// ============================================================

func TestNormalizeCopilotModelID(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		// 基本 dash → dot 转换
		{"dash to dot sonnet", "claude-sonnet-4-6", "claude-sonnet-4.6"},
		{"dash to dot opus", "claude-opus-4-6", "claude-opus-4.6"},
		{"dash to dot haiku", "claude-haiku-4-5", "claude-haiku-4.5"},
		// [1m] → -1m
		{"bracket 1m sonnet", "claude-sonnet-4-6[1m]", "claude-sonnet-4.6-1m"},
		{"bracket 1m opus", "claude-opus-4-6[1m]", "claude-opus-4.6-1m"},
		// 已归一化的不变
		{"already normalized", "claude-sonnet-4.6", "claude-sonnet-4.6"},
		{"already with 1m", "claude-opus-4.6-1m", "claude-opus-4.6-1m"},
		{"already haiku", "claude-haiku-4.5", "claude-haiku-4.5"},
		// 日期后缀去除
		{"date suffix haiku", "claude-haiku-4-5-20251001", "claude-haiku-4.5"},
		{"date suffix sonnet", "claude-sonnet-4-5-20250929", "claude-sonnet-4.5"},
		// 非 Claude 模型不变
		{"non-claude gpt-5", "gpt-5", "gpt-5"},
		{"non-claude gpt-4o-mini", "gpt-4o-mini", "gpt-4o-mini"},
		{"non-claude o3", "o3", "o3"},
		{"empty string", "", ""},
		// 旧版三位版本不变
		{"legacy 3-5-sonnet", "claude-3-5-sonnet", "claude-3-5-sonnet"},
		{"legacy 3-5-sonnet-date", "claude-3-5-sonnet-20241022", "claude-3-5-sonnet-20241022"},
		// 大小写不敏感
		{"case insensitive", "Claude-Sonnet-4-6", "Claude-Sonnet-4.6"},
		{"case insensitive 1m", "claude-sonnet-4-6[1M]", "claude-sonnet-4.6-1m"},
		// [1m] + 日期组合
		{"date + 1m", "claude-haiku-4-5-20251001[1m]", "claude-haiku-4.5-1m"},
		// dotted + [1m]
		{"dotted with 1m", "claude-sonnet-4.6[1m]", "claude-sonnet-4.6-1m"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := NormalizeCopilotModelID(tc.input)
			if result != tc.expect {
				t.Errorf("NormalizeCopilotModelID(%q) = %q, want %q", tc.input, result, tc.expect)
			}
		})
	}
}

// ============================================================
// NormalizeGitHubDomain 测试
// ============================================================

func TestNormalizeGitHubDomain(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		expect  string
		wantErr bool
	}{
		{"github.com", "github.com", "github.com", false},
		{"with https", "https://github.com", "github.com", false},
		{"with http", "http://github.com", "github.com", false},
		{"enterprise domain", "company.ghe.com", "company.ghe.com", false},
		{"enterprise with https", "https://company.ghe.com", "company.ghe.com", false},
		{"with trailing slash", "https://github.com/", "github.com", false},
		{"with path", "https://github.com/some/path", "github.com", false},
		{"with query", "github.com?foo=bar", "github.com", false},
		{"with fragment", "github.com#anchor", "github.com", false},
		{"case insensitive", "GitHub.COM", "github.com", false},
		{"with whitespace", "  github.com  ", "github.com", false},
		{"enterprise with http", "http://ghes.example.com", "ghes.example.com", false},
		{"with port", "https://ghes.example.com:8443", "ghes.example.com:8443", false},
		{"with userinfo (reject)", "https://user:pass@github.com", "", true},
		{"empty (reject)", "", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := NormalizeGitHubDomain(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Errorf("NormalizeGitHubDomain(%q) expected error, got nil", tc.input)
				}
				return
			}
			if err != nil {
				t.Errorf("NormalizeGitHubDomain(%q) unexpected error: %v", tc.input, err)
				return
			}
			if result != tc.expect {
				t.Errorf("NormalizeGitHubDomain(%q) = %q, want %q", tc.input, result, tc.expect)
			}
		})
	}
}

// ============================================================
// CompositeAccountID 测试
// ============================================================

func TestCompositeAccountID(t *testing.T) {
	tests := []struct {
		name   string
		domain string
		userID string
		expect string
	}{
		{"github.com user", "github.com", "12345", "12345"},
		{"GHES user", "company.ghe.com", "67890", "company.ghe.com:67890"},
		{"empty domain", "", "12345", ":12345"},
		{"empty userID", "github.com", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := CompositeAccountID(tc.domain, tc.userID)
			if result != tc.expect {
				t.Errorf("CompositeAccountID(%q, %q) = %q, want %q", tc.domain, tc.userID, result, tc.expect)
			}
		})
	}
}

// ============================================================
// dashesToDotInLastVersion 测试
// ============================================================

func TestDashesToDotInLastVersion(t *testing.T) {
	tests := []struct {
		input  string
		expect string
	}{
		{"claude-sonnet-4-6", "claude-sonnet-4.6"},
		{"claude-opus-4-6", "claude-opus-4.6"},
		{"claude-haiku-4-5", "claude-haiku-4.5"},
		{"claude-3-5-sonnet", ""},
		{"single-dash", ""},
		{"claude-sonnet-4-", ""},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result := dashesToDotInLastVersion(tc.input)
			if result != tc.expect {
				t.Errorf("dashesToDotInLastVersion(%q) = %q, want %q", tc.input, result, tc.expect)
			}
		})
	}
}

// ============================================================
// split1MSuffix 测试
// ============================================================

func TestSplit1MSuffix(t *testing.T) {
	tests := []struct {
		input      string
		expectBase string
		expectSuff bool
	}{
		{"claude-sonnet-4-6[1m]", "claude-sonnet-4-6", true},
		{"claude-sonnet-4-6-1m", "claude-sonnet-4-6", true},
		{"claude-sonnet-4-6", "claude-sonnet-4-6", false},
		{"claude-sonnet-4.6[1m]", "claude-sonnet-4.6", true},
		{"claude-sonnet-4.6", "claude-sonnet-4.6", false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			base, has := split1MSuffix(tc.input)
			if base != tc.expectBase || has != tc.expectSuff {
				t.Errorf("split1MSuffix(%q) = (%q, %v), want (%q, %v)", tc.input, base, has, tc.expectBase, tc.expectSuff)
			}
		})
	}
}

// ============================================================
// stripTrailingDate 测试
// ============================================================

func TestStripTrailingDate(t *testing.T) {
	tests := []struct {
		input  string
		expect string
	}{
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5"},
		{"claude-sonnet-4-5-20250929", "claude-sonnet-4-5"},
		{"claude-sonnet-4-6", "claude-sonnet-4-6"},
		{"claude-sonnet-4-6-202509290", "claude-sonnet-4-6-202509290"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result := stripTrailingDate(tc.input)
			if result != tc.expect {
				t.Errorf("stripTrailingDate(%q) = %q, want %q", tc.input, result, tc.expect)
			}
		})
	}
}

// ============================================================
// CopilotAPIBase 测试
// ============================================================

func TestCopilotAPIBase(t *testing.T) {
	tests := []struct {
		domain string
		expect string
	}{
		{"github.com", "https://api.githubcopilot.com"},
		{"company.ghe.com", "https://copilot-api.company.ghe.com"},
	}

	for _, tc := range tests {
		t.Run(tc.domain, func(t *testing.T) {
			result := CopilotAPIBase(tc.domain)
			if result != tc.expect {
				t.Errorf("CopilotAPIBase(%q) = %q, want %q", tc.domain, result, tc.expect)
			}
		})
	}
}

// ============================================================
// CopilotTargetURL 测试
// ============================================================

func TestCopilotTargetURL(t *testing.T) {
	tests := []struct {
		name     string
		provider *config.Provider
		expect   string
	}{
		{
			name: "github.com via CopilotBaseURL",
			provider: &config.Provider{
				CopilotBaseURL: "github.com",
				BaseURL:        "https://api.githubcopilot.com",
			},
			expect: "https://api.githubcopilot.com/chat/completions",
		},
		{
			name: "GHES domain",
			provider: &config.Provider{
				CopilotBaseURL: "company.ghe.com",
				BaseURL:        "https://api.githubcopilot.com",
			},
			expect: "https://copilot-api.company.ghe.com/chat/completions",
		},
		{
			name: "fallback to BaseURL when CopilotBaseURL empty",
			provider: &config.Provider{
				CopilotBaseURL: "",
				BaseURL:        "https://custom.copilot.com",
			},
			expect: "https://custom.copilot.com/chat/completions",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := CopilotTargetURL(tc.provider)
			if result != tc.expect {
				t.Errorf("CopilotTargetURL() = %q, want %q", result, tc.expect)
			}
		})
	}
}

// ============================================================
// CopilotTokenURL, GitHub URL helpers 测试
// ============================================================

func TestCopilotTokenURL(t *testing.T) {
	tests := []struct {
		domain string
		expect string
	}{
		{"github.com", "https://api.github.com/copilot_internal/v2/token"},
		{"company.ghe.com", "https://company.ghe.com/api/v3/copilot_internal/v2/token"},
	}

	for _, tc := range tests {
		t.Run(tc.domain, func(t *testing.T) {
			result := CopilotTokenURL(tc.domain)
			if result != tc.expect {
				t.Errorf("CopilotTokenURL(%q) = %q, want %q", tc.domain, result, tc.expect)
			}
		})
	}
}

func TestGithubOAuthTokenURL(t *testing.T) {
	if got := GithubOAuthTokenURL("github.com"); got != "https://github.com/login/oauth/access_token" {
		t.Errorf("GithubOAuthTokenURL(github.com) = %q, want https://github.com/login/oauth/access_token", got)
	}
}

func TestGithubDeviceCodeURL(t *testing.T) {
	if got := GithubDeviceCodeURL("github.com"); got != "https://github.com/login/device/code" {
		t.Errorf("GithubDeviceCodeURL(github.com) = %q, want https://github.com/login/device/code", got)
	}
}

func TestGithubUserURL(t *testing.T) {
	tests := []struct {
		domain string
		expect string
	}{
		{"github.com", "https://api.github.com/user"},
		{"company.ghe.com", "https://company.ghe.com/api/v3/user"},
	}

	for _, tc := range tests {
		t.Run(tc.domain, func(t *testing.T) {
			result := GithubUserURL(tc.domain)
			if result != tc.expect {
				t.Errorf("GithubUserURL(%q) = %q, want %q", tc.domain, result, tc.expect)
			}
		})
	}
}

func TestGithubClientID(t *testing.T) {
	if got := GithubClientID("github.com"); got != "Iv1.b507a08c87ecfe98" {
		t.Errorf("GithubClientID(github.com) = %q, want Iv1.b507a08c87ecfe98", got)
	}
	if got := GithubClientID("company.ghe.com"); got != "Ov23li8tweQw6odWQebz" {
		t.Errorf("GithubClientID(enterprise) = %q, want Ov23li8tweQw6odWQebz", got)
	}
}

// ============================================================
// InjectCopilotHeaders 测试
// ============================================================

func TestInjectCopilotHeaders(t *testing.T) {
	original := http.Header{}
	original.Set("Authorization", "Bearer placeholder")
	original.Set("Content-Type", "application/json")
	original.Set("x-anthropic-version", "2023-06-01")

	result := InjectCopilotHeaders(original, "test-copilot-token-123")

	if result.Get("Authorization") != "Bearer test-copilot-token-123" {
		t.Errorf("Authorization = %q, want Bearer test-copilot-token-123", result.Get("Authorization"))
	}
	if result.Get("Editor-Version") != CopilotEditorVersion {
		t.Errorf("Editor-Version = %q, want %q", result.Get("Editor-Version"), CopilotEditorVersion)
	}
	if result.Get("User-Agent") != CopilotUserAgent {
		t.Errorf("User-Agent = %q, want %q", result.Get("User-Agent"), CopilotUserAgent)
	}
	if result.Get("Copilot-Integration-Id") != CopilotIntegrationID {
		t.Errorf("Copilot-Integration-Id = %q, want %q", result.Get("Copilot-Integration-Id"), CopilotIntegrationID)
	}
	if result.Get("Openai-Intent") != "conversation-agent" {
		t.Errorf("Openai-Intent = %q, want conversation-agent", result.Get("Openai-Intent"))
	}
	if result.Get("x-anthropic-version") != "2023-06-01" {
		t.Errorf("x-anthropic-version should be preserved, got %q", result.Get("x-anthropic-version"))
	}
	if result.Get("X-Request-ID") == "" {
		t.Error("X-Request-ID should not be empty")
	}

	requiredHeaders := []string{
		"Authorization", "Content-Type", "Editor-Version",
		"Editor-Plugin-Version", "User-Agent", "Copilot-Integration-Id",
		"Openai-Intent", "X-Github-Api-Version", "X-Initiator",
		"X-Interaction-Type", "X-Request-ID", "X-Agent-Task-ID",
	}

	for _, h := range requiredHeaders {
		if result.Get(h) == "" {
			t.Errorf("Required header %q is empty", h)
		}
	}
}

// ============================================================
// Copilot 完整代理流程集成测试
// ============================================================

// TestCopilotSendRequest 验证 sendRequest 在 Copilot 模式下正确注入所有 header
// 使用 buildTargetURL (非 Copilot 路径) 来避免 CopilotTargetURL 硬编码 https://copilot-api.* 的问题
func TestCopilotSendRequest(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("Expected /chat/completions path, got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-copilot-token" {
			t.Errorf("Authorization = %q, want Bearer test-copilot-token", r.Header.Get("Authorization"))
		}

		copilotHeaders := map[string]string{
			"Editor-Version":         CopilotEditorVersion,
			"Editor-Plugin-Version":  CopilotPluginVersion,
			"User-Agent":             CopilotUserAgent,
			"Copilot-Integration-Id": CopilotIntegrationID,
			"Openai-Intent":          "conversation-agent",
						"X-Initiator":            "user",
			"X-Interaction-Type":     "conversation-agent",
		}
		for header, expectedValue := range copilotHeaders {
			if got := r.Header.Get(header); got == "" {
				t.Errorf("Missing required Copilot header %q", header)
			} else if got != expectedValue {
				t.Errorf("Header %q = %q, want %q", header, got, expectedValue)
			}
		}

		fingerprintHeaders := []string{"X-Request-ID", "X-Agent-Task-ID", "X-Vscode-User-Agent-Library-Version"}
		for _, h := range fingerprintHeaders {
			if r.Header.Get(h) == "" {
				t.Errorf("Missing fingerprint header %q", h)
			}
		}

		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
		}

		body, _ := io.ReadAll(r.Body)
		var reqBody map[string]interface{}
		if err := json.Unmarshal(body, &reqBody); err != nil {
			t.Errorf("Failed to parse request body: %v", err)
		}
		if model, ok := reqBody["model"].(string); !ok || model != "claude-sonnet-4.6" {
			t.Errorf("model = %q, want claude-sonnet-4.6", model)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":     "chatcmpl-123",
			"object": "chat.completion",
			"model":  "claude-sonnet-4.6",
			"choices": []map[string]interface{}{},
			"usage": map[string]interface{}{
				"prompt_tokens":     10,
				"completion_tokens": 20,
				"total_tokens":      30,
			},
		})
	}))
	defer mockServer.Close()

	// Provider without CopilotBaseURL so buildTargetURL uses BaseURL (the mock server URL).
	// Copilot behavior is tested via the copilotToken parameter to sendRequest.
	provider := &config.Provider{
		ID:             "test-copilot",
		Name:           "Test Copilot",
		BaseURL:        mockServer.URL,
		IsOpenAIFormat: true,
	}

	// Build the request body with an already-normalized model.
	// Model normalization is covered by TestNormalizeCopilotModelID;
	// this test focuses on sendRequest header injection.
	normalizedModel := NormalizeCopilotModelID("claude-sonnet-4-6")
	bodyBytes, _ := json.Marshal(map[string]interface{}{
		"model": normalizedModel,
		"messages": []map[string]interface{}{
			{"role": "user", "content": "Hello Copilot"},
		},
		"stream": false,
	})

	// targetURL uses provider.BaseURL (the mock server URL) — avoids CopilotTargetURL hardcoding https://
	targetURL := provider.BaseURL + "/v1/chat/completions"
	originalHeaders := http.Header{}
	originalHeaders.Set("Authorization", "Bearer sk-original")
	originalHeaders.Set("Content-Type", "application/json")
	originalHeaders.Set("x-anthropic-version", "2023-06-01")

	resp, err := sendRequest("POST", targetURL, originalHeaders, bodyBytes, provider, "test-copilot-token")
	if err != nil {
		t.Fatalf("sendRequest failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected 200, got %d", resp.StatusCode)
	}
}

// TestCopilotProxyFlowE2E 验证端到端代理：Claude 格式 → 格式转换 → Copilot header 注入 → 响应
func TestCopilotProxyFlowE2E(t *testing.T) {
	mockCopilot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Openai-Intent") != "conversation-agent" {
			t.Errorf("Missing Copilot header: Openai-Intent")
		}
		if r.Header.Get("Copilot-Integration-Id") != CopilotIntegrationID {
			t.Errorf("Missing Copilot header: Copilot-Integration-Id")
		}
		if r.Header.Get("Authorization") != "Bearer mock-copilot-token" {
			t.Errorf("Authorization = %q, want Bearer mock-copilot-token", r.Header.Get("Authorization"))
		}
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("Expected /chat/completions path, got %s", r.URL.Path)
		}

		body, _ := io.ReadAll(r.Body)
		var reqBody map[string]interface{}
		json.Unmarshal(body, &reqBody)
		if reqBody["model"] == nil {
			t.Error("Request body missing model")
		}
		if reqBody["messages"] == nil {
			t.Error("Request body missing messages")
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      "chatcmpl-copilot-123",
			"object":  "chat.completion",
			"model":   "claude-sonnet-4.6",
			"choices": []map[string]interface{}{},
			"usage": map[string]interface{}{
				"prompt_tokens":     50,
				"completion_tokens": 100,
				"total_tokens":      150,
			},
		})
	}))
	defer mockCopilot.Close()

	// Provider without CopilotBaseURL so TargetURL uses BaseURL (mock server).
	// The copilotToken parameter to sendRequest tests the Copilot header injection path.
	provider := &config.Provider{
		ID:             "test-copilot-e2e",
		Name:           "Test Copilot E2E",
		BaseURL:        mockCopilot.URL,
		IsOpenAIFormat: true,
	}

	claudeBody := map[string]interface{}{
		"model": "claude-sonnet-4-6",
		"messages": []map[string]interface{}{
			{"role": "user", "content": "Hello, this is a test message"},
		},
		"max_tokens": 4096,
		"stream":     false,
	}
	bodyBytes, _ := json.Marshal(claudeBody)

	processed, err := processRequestBody(bodyBytes, provider, false, "/v1/messages", "")
	if err != nil {
		t.Fatalf("processRequestBody failed: %v", err)
	}

	if !strings.Contains(processed.TargetURL, "/chat/completions") {
		t.Errorf("TargetURL = %q, should contain /chat/completions", processed.TargetURL)
	}

	var processedBody map[string]interface{}
	if err := json.Unmarshal(processed.BodyBytes, &processedBody); err != nil {
		t.Fatalf("Failed to parse processed body: %v", err)
	}
	// Model is resolved but NOT normalized (no CopilotBaseURL)
	model, _ := processedBody["model"].(string)
	if model != "claude-sonnet-4-6" {
		t.Errorf("Model after processing = %q, want claude-sonnet-4-6", model)
	}

	headers := http.Header{}
	headers.Set("Authorization", "Bearer test-key")
	headers.Set("Content-Type", "application/json")
	headers.Set("x-anthropic-version", "2023-06-01")

	resp, err := sendRequest("POST", processed.TargetURL, headers, processed.BodyBytes, provider, "mock-copilot-token")
	if err != nil {
		t.Fatalf("sendRequest failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected 200, got %d", resp.StatusCode)
	}
}

// TestCopilotSendRequestWithAnthropicHeaders 验证 Anthropic header 被保留
func TestCopilotSendRequestWithAnthropicHeaders(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-anthropic-version") != "2023-06-01" {
			t.Errorf("x-anthropic-version = %q, want 2023-06-01", r.Header.Get("x-anthropic-version"))
		}
		if r.Header.Get("Authorization") == "Bearer original-key" {
			t.Error("Original Authorization should be replaced")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"test"}`))
	}))
	defer mockServer.Close()

	provider := &config.Provider{BaseURL: mockServer.URL}
	bodyBytes := []byte(`{"model":"test"}`)
	headers := http.Header{}
	headers.Set("Authorization", "Bearer original-key")
	headers.Set("x-anthropic-version", "2023-06-01")
	headers.Set("x-custom-header", "custom-value")

	resp, err := sendRequest("POST", mockServer.URL, headers, bodyBytes, provider, "copilot-token-here")
	if err != nil {
		t.Fatalf("sendRequest failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected 200, got %d", resp.StatusCode)
	}
}

// TestCopilotModelResolutionChain 验证模型解析链：ResolveModel → NormalizeCopilotModelID
func TestCopilotModelResolutionChain(t *testing.T) {
	provider := &config.Provider{
		SonnetModel: "claude-sonnet-4-6",
		OpusModel:   "claude-opus-4-7",
		HaikuModel:  "claude-haiku-4-5",
		FastModel:   "claude-haiku-4-5",
	}

	tests := []struct {
		name           string
		clientModelKey string
		expectedModel  string
	}{
		{"sonnet resolution + normalization", "sonnet_model", "claude-sonnet-4.6"},
		{"opus resolution + normalization", "opus_model", "claude-opus-4.7"},
		{"haiku resolution + normalization", "haiku_model", "claude-haiku-4.5"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resolved := provider.ResolveModel(tc.clientModelKey)
			normalized := NormalizeCopilotModelID(resolved)
			if normalized != tc.expectedModel {
				t.Errorf("Chain result = %q, want %q (resolved=%q)", normalized, tc.expectedModel, resolved)
			}
		})
	}
}

// ============================================================
// GHES 和边缘情况测试
// ============================================================

func TestCopilotProxyGHES(t *testing.T) {
	apiBase := CopilotAPIBase("company.ghe.com")
	if apiBase != "https://copilot-api.company.ghe.com" {
		t.Errorf("GHES Copilot API base = %q, want https://copilot-api.company.ghe.com", apiBase)
	}

	tokenURL := CopilotTokenURL("company.ghe.com")
	if tokenURL != "https://company.ghe.com/api/v3/copilot_internal/v2/token" {
		t.Errorf("GHES token URL = %q", tokenURL)
	}

	provider := &config.Provider{
		BaseURL:        "https://api.githubcopilot.com",
		CopilotBaseURL: "company.ghe.com",
	}
	target := CopilotTargetURL(provider)
	if target != "https://copilot-api.company.ghe.com/chat/completions" {
		t.Errorf("GHES target URL = %q, want https://copilot-api.company.ghe.com/chat/completions", target)
	}

	if GithubClientID("company.ghe.com") == GithubClientID("github.com") {
		t.Error("GHES client ID should differ from github.com")
	}
}

func TestCopilotNormalizeModelPreservesOpenAIModels(t *testing.T) {
	openAIModels := []string{
		"gpt-5.4", "gpt-5.3-codex", "gpt-4o", "o3", "o4-mini",
	}

	for _, model := range openAIModels {
		t.Run(model, func(t *testing.T) {
			result := NormalizeCopilotModelID(model)
			if result != model {
				t.Errorf("NormalizeCopilotModelID(%q) = %q, want unchanged", model, result)
			}
		})
	}
}

func TestCopilotSendRequestWithEmptyBody(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer mockServer.Close()

	provider := &config.Provider{BaseURL: mockServer.URL}
	resp, err := sendRequest("POST", mockServer.URL, http.Header{}, []byte{}, provider, "token")
	if err != nil {
		t.Fatalf("sendRequest with empty body failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected 200, got %d", resp.StatusCode)
	}
}

func TestCopilotInjectHeadersPreservesContentType(t *testing.T) {
	original := http.Header{}
	original.Set("Content-Type", "application/json")
	result := InjectCopilotHeaders(original, "token")
	if result.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", result.Get("Content-Type"))
	}
}

func TestCopilotSendRequestBodyCorrectness(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("Failed to parse body: %v", err)
		}
		messages, ok := req["messages"].([]interface{})
		if !ok || len(messages) != 1 {
			t.Fatalf("Expected 1 message, got %d", len(messages))
		}
		msg := messages[0].(map[string]interface{})
		if msg["role"] != "user" {
			t.Errorf("Role = %q, want user", msg["role"])
		}
		if msg["content"] != "Hello Copilot" {
			t.Errorf("Content = %q, want 'Hello Copilot'", msg["content"])
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"ok"}`))
	}))
	defer mockServer.Close()

	provider := &config.Provider{BaseURL: mockServer.URL}
	bodyBytes := []byte(`{"model":"test","messages":[{"role":"user","content":"Hello Copilot"}]}`)
	resp, err := sendRequest("POST", mockServer.URL, http.Header{}, bodyBytes, provider, "token")
	if err != nil {
		t.Fatalf("sendRequest failed: %v", err)
	}
	defer resp.Body.Close()
}

func TestCopilotProcessRequestBodyCopilotProvider(t *testing.T) {
	// Provider without CopilotBaseURL so TargetURL uses BaseURL (mock-friendly).
	// Model normalization is covered by TestNormalizeCopilotModelID and
	// the full flow by TestCopilotSendRequest.
	provider := &config.Provider{
		ID:             "test-copilot",
		BaseURL:        "https://api.githubcopilot.com",
		IsOpenAIFormat: true,
		SonnetModel:    "claude-sonnet-4-6",
	}

	bodyBytes, _ := json.Marshal(map[string]interface{}{
		"model": "sonnet_model",
		"messages": []map[string]interface{}{
			{"role": "user", "content": "Hello"},
		},
	})

	processed, err := processRequestBody(bodyBytes, provider, true, "/v1/chat/completions", "")
	if err != nil {
		t.Fatalf("processRequestBody failed: %v", err)
	}

	if !strings.Contains(processed.TargetURL, "/v1/chat/completions") {
		t.Errorf("TargetURL = %q, should contain /v1/chat/completions", processed.TargetURL)
	}

	var reqBody map[string]interface{}
	json.Unmarshal(processed.BodyBytes, &reqBody)
	model, _ := reqBody["model"].(string)
	if model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want claude-sonnet-4-6", model)
	}
}

// TestCopilotProcessRequestBodyWithFormatConversion 验证 Claude→OpenAI 格式转换
// (Copilot URL/model normalization 由 TestNormalizeCopilotModelID 覆盖)
func TestCopilotProcessRequestBodyWithFormatConversion(t *testing.T) {
	provider := &config.Provider{
		BaseURL:        "https://api.githubcopilot.com",
		IsOpenAIFormat: true, // provider 需要 OpenAI 格式
		DefaultModel:   "claude-sonnet-4-6",
	}

	// 传入 Claude 格式（isIncomingOpenAIFormat=false）
	claudeBody := map[string]interface{}{
		"model": "default_model",
		"messages": []map[string]interface{}{
			{"role": "user", "content": "Hello"},
		},
		"max_tokens": 4096,
		"stream":     false,
	}
	bodyBytes, _ := json.Marshal(claudeBody)

	processed, err := processRequestBody(bodyBytes, provider, false, "/v1/messages", "")
	if err != nil {
		t.Fatalf("processRequestBody failed: %v", err)
	}

	// 格式转换后 URL 应为 /v1/chat/completions
	expectedURL := "https://api.githubcopilot.com/v1/chat/completions"
	if processed.TargetURL != expectedURL {
		t.Errorf("TargetURL = %q, want %q", processed.TargetURL, expectedURL)
	}

	var reqBody map[string]interface{}
	json.Unmarshal(processed.BodyBytes, &reqBody)
	// Claude format 被转换为 OpenAI format
	if _, ok := reqBody["messages"]; !ok {
		t.Error("Body should have messages (OpenAI format)")
	}
	// model 已解析但未归一化（无 CopilotBaseURL）
	model, _ := reqBody["model"].(string)
	if model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want claude-sonnet-4-6", model)
	}
}
