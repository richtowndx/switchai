package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"switchai/appdata"
	"switchai/config"
)

// initTestConfig initializes the config using the real .switchai directory
func initTestConfig(t *testing.T) {
	t.Helper()
	os.Args = []string{"switchai"}

	// Find project root (where .switchai/config.db lives)
	wd, _ := os.Getwd()
	projectRoot := wd
	if _, err := os.Stat(filepath.Join(wd, ".switchai", "config.db")); os.IsNotExist(err) {
		// CWD is likely the package directory, go up one level
		parent := filepath.Dir(wd)
		if _, err2 := os.Stat(filepath.Join(parent, ".switchai", "config.db")); err2 == nil {
			projectRoot = parent
		}
	}

	// Change to project root so relative paths resolve correctly
	oldDir, _ := os.Getwd()
	os.Chdir(projectRoot)
	t.Cleanup(func() { os.Chdir(oldDir) })

	if err := appdata.Init(); err != nil {
		t.Fatalf("appdata.Init() failed: %v", err)
	}

	// Verify the config.db exists
	configPath := filepath.Join(appdata.GetDataDir(), "config.db")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Skipf("config.db not found at %s, skipping integration test", configPath)
	}

	if err := config.Init(); err != nil {
		t.Fatalf("config.Init() failed: %v", err)
	}
}

// TestCopilotRealIntegration 使用真实的 config.db 验证 Copilot 代理流程
// 包含: token 自动刷新 → header 注入 → 真实 copilot API 请求 → 响应验证
//
// 运行方式: go test ./proxy/ -run TestCopilotRealIntegration -v -count=1 -timeout 60s
func TestCopilotRealIntegration(t *testing.T) {
	// 1. 初始化配置（连接真实 config.db）
	initTestConfig(t)
	defer config.Shutdown()

	cfg := config.GetConfig()

	// 2. 查找 Copilot 提供商
	var copilotProvider *config.Provider
	for i := range cfg.Providers {
		if cfg.Providers[i].IsCopilot() {
			copilotProvider = &cfg.Providers[i]
			break
		}
	}
	if copilotProvider == nil {
		t.Skip("未找到 Copilot 提供商，跳过集成测试")
	}

	t.Logf("找到 Copilot 提供商: name=%s, base_url=%s, copilot_base_url=%s, account_id=%s",
		copilotProvider.Name, copilotProvider.BaseURL,
		copilotProvider.CopilotBaseURL, copilotProvider.CopilotAuthAccountID)

	// 3. 刷新/获取 Copilot token（自动处理过期）
	copilotToken := RefreshCopilotToken(copilotProvider)
	if copilotToken == "" {
		t.Fatalf("无法获取 Copilot token（可能未认证或 token 刷新失败）")
	}
	t.Logf("Copilot token 获取成功 (长度: %d)", len(copilotToken))

	// 4. 构建请求体（OpenAI 格式，copilot 使用 OpenAI 兼容接口）
	// 使用 gpt-4o 作为测试模型（兼容 /chat/completions 端点）
	model := "gpt-4o"
	model = NormalizeCopilotModelID(model)
	t.Logf("使用模型: %s (provider sonnet=%s, default=%s)",
		model, copilotProvider.SonnetModel, copilotProvider.DefaultModel)

	reqBody := map[string]interface{}{
		"model": model,
		"messages": []map[string]interface{}{
			{"role": "user", "content": "Say hello in one word."},
		},
		"max_tokens": 10,
		"stream":     false,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	// 5. 发送请求到真实 Copilot API
	targetURL := CopilotTargetURL(copilotProvider)
	t.Logf("目标 URL: %s", targetURL)

	originalHeaders := http.Header{}
	originalHeaders.Set("Authorization", "Bearer placeholder")
	originalHeaders.Set("Content-Type", "application/json")

	resp, err := sendRequest("POST", targetURL, originalHeaders, bodyBytes, copilotProvider, copilotToken)
	if err != nil {
		t.Fatalf("sendRequest 失败: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	t.Logf("响应状态: %d", resp.StatusCode)
	t.Logf("响应体 (前 500 字节): %s", truncate(string(respBody), 500))

	// 6. 验证响应
	if resp.StatusCode != http.StatusOK {
		t.Errorf("期望 200, 实际 %d, 响应: %s", resp.StatusCode, string(respBody))
	}

	var respJSON map[string]interface{}
	if err := json.Unmarshal(respBody, &respJSON); err != nil {
		t.Fatalf("响应 JSON 解析失败: %v", err)
	}

	// 验证基本结构
	if respJSON["id"] == nil {
		t.Error("响应缺少 id 字段")
	}
	if respJSON["model"] == nil {
		t.Error("响应缺少 model 字段")
	}

	// 验证有 AI 回复
	choices, ok := respJSON["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		t.Error("响应缺少 choices")
	} else {
		choice := choices[0].(map[string]interface{})
		msg := choice["message"].(map[string]interface{})
		content, _ := msg["content"].(string)
		t.Logf("AI 回复: %s", content)
		if content == "" {
			t.Error("AI 回复为空")
		}
	}

	t.Log("Copilot 集成测试通过!")
}

// TestCopilotTokenAutoRefresh 验证 token 自动刷新机制
// 检查 DB 中的 token 状态，如果即将过期则触发刷新
func TestCopilotTokenAutoRefresh(t *testing.T) {
	initTestConfig(t)
	defer config.Shutdown()

	cfg := config.GetConfig()

	var copilotProvider *config.Provider
	for i := range cfg.Providers {
		if cfg.Providers[i].IsCopilot() {
			copilotProvider = &cfg.Providers[i]
			break
		}
	}
	if copilotProvider == nil {
		t.Skip("未找到 Copilot 提供商")
	}

	store := config.GetCopilotTokenStore()
	accountID := copilotProvider.CopilotAuthAccountID
	if accountID == "" {
		var err error
		accountID, err = store.GetDefaultAccountID()
		if err != nil || accountID == "" {
			t.Skip("无关联账号")
		}
	}

	token, err := store.GetCopilotTokenByAccountID(accountID)
	if err != nil || token == nil {
		t.Fatalf("无法获取 token: %v", err)
	}

	expiresAt := time.Unix(token.ExpiresAt, 0)
	remaining := time.Until(expiresAt)
	t.Logf("Token 过期时间: %s (剩余: %v)", expiresAt.Format(time.RFC3339), remaining)

	wasExpiring := isTokenExpiringSoon(token.ExpiresAt)
	t.Logf("是否即将过期: %v", wasExpiring)

	// 触发刷新
	newToken := RefreshCopilotToken(copilotProvider)
	if newToken == "" {
		t.Fatalf("RefreshCopilotToken 返回空")
	}

	// 验证刷新后的状态
	token2, err := store.GetCopilotTokenByAccountID(accountID)
	if err != nil || token2 == nil {
		t.Fatalf("刷新后获取 token 失败: %v", err)
	}

	expiresAt2 := time.Unix(token2.ExpiresAt, 0)
	remaining2 := time.Until(expiresAt2)
	t.Logf("刷新后过期时间: %s (剩余: %v)", expiresAt2.Format(time.RFC3339), remaining2)

	// 刷新后应该有足够时间（至少 5 分钟）
	if remaining2 < 5*time.Minute {
		t.Errorf("刷新后 token 剩余时间过短: %v", remaining2)
	}

	t.Log("Token 自动刷新验证通过!")
}

// TestCopilotProxyWithProxyURL 验证通过代理发送 Copilot 请求
// 仅在环境变量 COPILOT_PROXY_URL 设置时运行
func TestCopilotProxyWithProxyURL(t *testing.T) {
	proxyURL := os.Getenv("COPILOT_PROXY_URL")
	if proxyURL == "" {
		t.Skip("COPILOT_PROXY_URL 未设置，跳过代理测试")
	}

	initTestConfig(t)
	defer config.Shutdown()

	cfg := config.GetConfig()

	var copilotProvider *config.Provider
	for i := range cfg.Providers {
		if cfg.Providers[i].IsCopilot() {
			copilotProvider = &cfg.Providers[i]
			break
		}
	}
	if copilotProvider == nil {
		t.Skip("未找到 Copilot 提供商")
	}

	copilotToken := RefreshCopilotToken(copilotProvider)
	if copilotToken == "" {
		t.Fatalf("无法获取 Copilot token")
	}

	model := copilotProvider.SonnetModel
	if model == "" {
		model = copilotProvider.DefaultModel
	}
	if model == "" {
		model = "gpt-4o"
	}
	model = NormalizeCopilotModelID(model)

	reqBody := map[string]interface{}{
		"model": model,
		"messages": []map[string]interface{}{
			{"role": "user", "content": "Say hello in one word."},
		},
		"max_tokens": 10,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	// 使用代理创建 HTTP client
	proxyFunc := func(req *http.Request) (*url.URL, error) {
		return url.Parse(proxyURL)
	}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: proxyFunc,
		},
		Timeout: 30 * time.Second,
	}

	targetURL := CopilotTargetURL(copilotProvider)
	req, _ := http.NewRequest("POST", targetURL, bytes.NewReader(bodyBytes))
	for k, v := range InjectCopilotHeaders(http.Header{}, copilotToken) {
		for _, val := range v {
			req.Header.Add(k, val)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("代理请求失败: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	t.Logf("代理响应状态: %d", resp.StatusCode)
	t.Logf("代理响应体: %s", truncate(string(body), 300))

	if resp.StatusCode != http.StatusOK {
		t.Errorf("期望 200, 实际 %d", resp.StatusCode)
	}

	t.Log("通过代理的 Copilot 请求测试通过!")
}

// TestCopilotStreamRealIntegration 验证 Copilot 流式请求
func TestCopilotStreamRealIntegration(t *testing.T) {
	if os.Getenv("RUN_STREAM_TEST") == "" {
		t.Skip("RUN_STREAM_TEST 未设置，跳过流式测试（避免消耗配额）")
	}

	initTestConfig(t)
	defer config.Shutdown()

	cfg := config.GetConfig()

	var copilotProvider *config.Provider
	for i := range cfg.Providers {
		if cfg.Providers[i].IsCopilot() {
			copilotProvider = &cfg.Providers[i]
			break
		}
	}
	if copilotProvider == nil {
		t.Skip("未找到 Copilot 提供商")
	}

	copilotToken := RefreshCopilotToken(copilotProvider)
	if copilotToken == "" {
		t.Fatalf("无法获取 Copilot token")
	}

	model := copilotProvider.SonnetModel
	if model == "" {
		model = copilotProvider.DefaultModel
	}
	if model == "" {
		model = "gpt-4o"
	}
	model = NormalizeCopilotModelID(model)

	reqBody := map[string]interface{}{
		"model": model,
		"messages": []map[string]interface{}{
			{"role": "user", "content": "Say hello"},
		},
		"max_tokens": 10,
		"stream":     true,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	targetURL := CopilotTargetURL(copilotProvider)
	originalHeaders := http.Header{}
	originalHeaders.Set("Authorization", "Bearer placeholder")

	resp, err := sendRequest("POST", targetURL, originalHeaders, bodyBytes, copilotProvider, copilotToken)
	if err != nil {
		t.Fatalf("sendRequest 失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("期望 200, 实际 %d: %s", resp.StatusCode, string(body))
	}

	ct := resp.Header.Get("Content-Type")
	t.Logf("Content-Type: %s", ct)

	body, _ := io.ReadAll(resp.Body)
	t.Logf("流式响应 (前 500 字节): %s", truncate(string(body), 500))

	if len(body) == 0 {
		t.Error("流式响应体为空")
	}

	t.Log("Copilot 流式请求测试通过!")
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + fmt.Sprintf("... (truncated, total %d bytes)", len(s))
}
