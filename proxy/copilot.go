package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"switchai/config"

	"github.com/google/uuid"
)

// ============================================================
// Copilot HTTP Header 常量
// ============================================================

const (
	CopilotEditorVersion     = "vscode/1.110.1"
	CopilotPluginVersion     = "copilot-chat/0.38.2"
	CopilotUserAgent         = "GitHubCopilotChat/0.38.2"
	CopilotIntegrationID     = "vscode-chat"
	CopilotAPIVersion        = "2025-10-01"
)

// InjectCopilotHeaders 在请求头中注入 Copilot 所需的 header
// 端口自 cc-switch: proxy/providers/claude.rs get_auth_headers (GitHubCopilot 分支)
// 返回修改后的 headers（不改变原始值）
func InjectCopilotHeaders(original http.Header, copilotToken string) http.Header {
	h := http.Header{}
	for k, v := range original {
		// 跳过原始 Authorization（用 Copilot token 替换）
		if strings.EqualFold(k, "Authorization") {
			continue
		}
		// 跳过 Copilot fingerprint headers（避免重复）
		switch strings.ToLower(k) {
		case "editor-version", "editor-plugin-version", "user-agent",
			"copilot-integration-id", "openai-intent", "openai-organization",
			"x-initiator", "x-interaction-type", "x-request-id",
			"x-agent-task-id", "x-vscode-user-agent-library-version",
			"x-github-api-version":
			continue
		}
		for _, val := range v {
			h.Add(k, val)
		}
	}

	// 注入 Copilot Bearer token
	h.Set("Authorization", "Bearer "+copilotToken)
	h.Set("Content-Type", "application/json")

	// 注入 Copilot 必需 header（对齐 cc-switch claude.rs GitHubCopilot 分支）
	h.Set("Editor-Version", CopilotEditorVersion)
	h.Set("Editor-Plugin-Version", CopilotPluginVersion)
	h.Set("User-Agent", CopilotUserAgent)
	h.Set("Copilot-Integration-Id", CopilotIntegrationID)
	h.Set("Openai-Intent", "conversation-agent")
	h.Set("X-Github-Api-Version", CopilotAPIVersion)

	// Copilot 指纹 header
	requestID := uuid.New().String()
	h.Set("X-Initiator", "user")
	h.Set("X-Interaction-Type", "conversation-agent")
	h.Set("X-Request-ID", requestID)
	h.Set("X-Agent-Task-ID", requestID)
	h.Set("X-Vscode-User-Agent-Library-Version", "electron-fetch")

	return h
}

// ============================================================
// Copilot 模型 ID 归一化
// 端口自 cc-switch: copilot_model_map.rs
//
// Copilot 上游只接受 dot 形式的 Claude 4.x 模型 ID（如 claude-sonnet-4.6），
// 而 Claude Code 客户端发出 dash 形式（如 claude-sonnet-4-6, claude-sonnet-4-6[1m]）。
// ============================================================

// normalizeCopilotModelID 将客户端 model ID 归一化为 Copilot upstream 接受的形式。
// 返回归一化后的模型名，如果不需要变换则返回原值。
func NormalizeCopilotModelID(clientID string) string {
	trimmed := strings.TrimSpace(clientID)
	if len(trimmed) < 8 || !strings.EqualFold(trimmed[:7], "claude-") {
		return clientID
	}

	has1mBracket := strings.HasSuffix(strings.ToLower(trimmed), "[1m]")

	// Fast path: 已含点 + 不带 [1m] → 已归一化
	if strings.Contains(trimmed, ".") && !has1mBracket {
		return clientID
	}

	base, has1mSuffix := split1MSuffix(trimmed)
	stripped := stripTrailingDate(base)
	dotted := dashesToDotInLastVersion(stripped)

	if dotted == "" && !has1mSuffix {
		return clientID
	}

	var candidate string
	if dotted != "" {
		candidate = dotted
	} else {
		candidate = stripped
	}
	if has1mSuffix {
		candidate += "-1m"
	}
	if candidate == trimmed {
		return clientID
	}
	return candidate
}

// split1MSuffix 分离 [1m] 或 -1m 后缀
func split1MSuffix(id string) (string, bool) {
	lower := strings.ToLower(id)
	if strings.HasSuffix(lower, "[1m]") {
		return id[:len(id)-4], true
	}
	if strings.HasSuffix(lower, "-1m") {
		return id[:len(id)-3], true
	}
	return id, false
}

// stripTrailingDate 移除末尾的 8 位数字日期后缀（如 -20251001）
func stripTrailingDate(id string) string {
	lastDash := strings.LastIndex(id, "-")
	if lastDash < 0 {
		return id
	}
	suffix := id[lastDash+1:]
	if len(suffix) == 8 {
		for _, c := range suffix {
			if c < '0' || c > '9' {
				return id
			}
		}
		return id[:lastDash]
	}
	return id
}

// dashesToDotInLastVersion 把 …-X-Y（X、Y 都是纯数字的末两段）变成 …-X.Y
// 例如 claude-sonnet-4-6 → claude-sonnet-4.6
// 返回空字符串表示模式不匹配
func dashesToDotInLastVersion(id string) string {
	lastDash := strings.LastIndex(id, "-")
	if lastDash < 0 {
		return ""
	}
	lastSegment := id[lastDash+1:]
	if lastSegment == "" {
		return ""
	}
	for _, c := range lastSegment {
		if c < '0' || c > '9' {
			return ""
		}
	}
	head := id[:lastDash]
	prevDash := strings.LastIndex(head, "-")
	if prevDash < 0 {
		return ""
	}
	prevSegment := head[prevDash+1:]
	if prevSegment == "" {
		return ""
	}
	for _, c := range prevSegment {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return head + "." + lastSegment
}

// normalizeGitHubDomain 归一化 GitHub 域名
// 端口自 cc-switch: normalize_github_domain()
func NormalizeGitHubDomain(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	// 剥离协议
	if prefix, ok := strings.CutPrefix(s, "https://"); ok {
		s = prefix
	} else if prefix, ok := strings.CutPrefix(s, "http://"); ok {
		s = prefix
	}
	// 取 host 部分
	host := strings.SplitN(s, "/", 2)[0]
	host = strings.SplitN(host, "?", 2)[0]
	host = strings.SplitN(host, "#", 2)[0]
	// 拒绝 userinfo
	if strings.Contains(host, "@") {
		return "", ErrInvalidDomain
	}
	normalized := strings.ToLower(host)
	if normalized == "" {
		return "", ErrInvalidDomain
	}
	return normalized, nil
}

// ErrInvalidDomain 表示无效的 GitHub 域名
var ErrInvalidDomain = &invalidDomainError{msg: "invalid github domain"}

type invalidDomainError struct {
	msg string
}

func (e *invalidDomainError) Error() string { return e.msg }

// compositeAccountID 生成复合账号 ID
// github.com 账号保持原格式，GHES 账号使用 domain:user_id 格式
func CompositeAccountID(domain, userID string) string {
	if domain == "github.com" {
		return userID
	}
	return domain + ":" + userID
}

// copilotAPIBase 返回 Copilot API 基础地址
func CopilotAPIBase(domain string) string {
	if domain == "github.com" {
		return "https://api.githubcopilot.com"
	}
	return "https://copilot-api." + domain
}

// copilotTokenURL 返回获取 Copilot token 的 URL
func CopilotTokenURL(domain string) string {
	if domain == "github.com" {
		return "https://api.github.com/copilot_internal/v2/token"
	}
	return "https://" + domain + "/api/v3/copilot_internal/v2/token"
}

// githubOAuthTokenURL 返回 GitHub OAuth token URL
func GithubOAuthTokenURL(domain string) string {
	return "https://" + domain + "/login/oauth/access_token"
}

// githubDeviceCodeURL 返回 GitHub 设备码 URL
func GithubDeviceCodeURL(domain string) string {
	return "https://" + domain + "/login/device/code"
}

// githubUserURL 返回 GitHub user API URL
func GithubUserURL(domain string) string {
	if domain == "github.com" {
		return "https://api.github.com/user"
	}
	return "https://" + domain + "/api/v3/user"
}

// githubClientID 根据域名选择 OAuth 客户端 ID
func GithubClientID(domain string) string {
	if domain == "github.com" {
		return "Iv1.b507a08c87ecfe98"
	}
	return "Ov23li8tweQw6odWQebz"
}

// Token refresh buffer: refresh 60 seconds before expiry
const tokenRefreshBufferSeconds int64 = 60

// resolveCopilotToken 为 Copilot 提供商解析有效的 Copilot token
func ResolveCopilotToken(provider *config.Provider) string {
	if !provider.IsCopilot() {
		return ""
	}

	store := config.GetCopilotTokenStore()

	// 确定使用哪个账号
	accountID := provider.CopilotAuthAccountID
	if accountID == "" {
		// 使用默认账号
		var err error
		accountID, err = store.GetDefaultAccountID()
		if err != nil || accountID == "" {
			return ""
		}
	}

	token, err := store.GetCopilotTokenByAccountID(accountID)
	if err != nil || token == nil {
		return ""
	}

	// 如果 token 不需要刷新，直接返回
	// 注意：实际刷新由 OAuth API endpoints 处理
	return token.CopilotToken
}

// copilotTargetURL 返回 Copilot 请求的目标 URL
// Copilot API 端点不含 /v1 前缀: https://api.githubcopilot.com/chat/completions
// 端口自 cc-switch: forwarder.rs rewrite_claude_transform_endpoint
func CopilotTargetURL(provider *config.Provider) string {
	domain := provider.CopilotBaseURL
	if domain == "" {
		return provider.BaseURL + "/chat/completions"
	}
	return CopilotAPIBase(domain) + "/chat/completions"
}

// copilotChatModels lists models known to work with /chat/completions.
// Models not in this list (e.g. codex models) are remapped to a compatible fallback.
var copilotChatModels = map[string]bool{
	"gpt-4o":                 true,
	"gpt-4o-mini":            true,
	"gpt-4.1":                true,
	"gpt-5-mini":             true,
	"gpt-5.2":                true,
	"gpt-5.4":                true,
	"gpt-5.4-mini":           true,
	"o3":                     true,
	"gemini-2.5-pro":         true,
	"gemini-3-flash-preview": true,
	"gemini-3.1-pro-preview": true,
	"grok-code-fast-1":       true,
	"gpt-41-copilot":         true,
}

// ResolveCopilotModel ensures the model is compatible with /chat/completions.
// If the resolved model is a codex/non-chat model, falls back to a chat-compatible one.
func ResolveCopilotModel(resolved string) string {
	lower := strings.ToLower(strings.TrimSpace(resolved))
	if copilotChatModels[lower] {
		return resolved
	}
	// Contains "codex" -> not a chat model
	if strings.Contains(lower, "codex") {
		log.Printf("[Copilot] Model %q not compatible with /chat/completions, falling back to gpt-4o", resolved)
		return "gpt-4o"
	}
	// Unknown model - let upstream decide
	return resolved
}

// ============================================================
// Copilot Token 自动刷新
// 端口自 cc-switch: copilot_auth.rs fetch_copilot_token_with_github_token
// ============================================================

// tokenRefreshMu 防止并发刷新同一账号的 token
var tokenRefreshMu sync.Mutex

// RefreshCopilotToken 检查 copilot token 是否即将过期，如果需要则自动刷新。
// 使用 provider 关联账号的 GitHub OAuth token 换取新的 Copilot token。
// 返回有效的 copilot token，如果刷新失败返回空字符串。
func RefreshCopilotToken(provider *config.Provider) string {
	if !provider.IsCopilot() {
		return ""
	}

	store := config.GetCopilotTokenStore()

	accountID := provider.CopilotAuthAccountID
	if accountID == "" {
		var err error
		accountID, err = store.GetDefaultAccountID()
		if err != nil || accountID == "" {
			return ""
		}
	}

	token, err := store.GetCopilotTokenByAccountID(accountID)
	if err != nil || token == nil {
		return ""
	}

	// 未过期（含 60 秒缓冲），直接返回
	if !isTokenExpiringSoon(token.ExpiresAt) {
		return token.CopilotToken
	}

	// 需要刷新：加锁防并发
	tokenRefreshMu.Lock()
	defer tokenRefreshMu.Unlock()

	// double-check：等锁期间可能已刷新
	token, err = store.GetCopilotTokenByAccountID(accountID)
	if err != nil || token == nil {
		return ""
	}
	if !isTokenExpiringSoon(token.ExpiresAt) {
		return token.CopilotToken
	}

	// 执行刷新
	log.Printf("[Copilot] Token 即将过期/已过期，自动刷新 (account: %s)", accountID)
	newToken, err := fetchNewCopilotToken(token.GitHubDomain, token.GitHubToken)
	if err != nil {
		log.Printf("[Copilot] Token 刷新失败: %v", err)
		return token.CopilotToken // 返回旧 token，可能还能用
	}

	// 更新 DB
	token.CopilotToken = newToken.Token
	token.ExpiresAt = newToken.ExpiresAt
	token.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := store.SaveCopilotToken(token); err != nil {
		log.Printf("[Copilot] Token 保存失败: %v", err)
	}

	log.Printf("[Copilot] Token 刷新成功, 新过期时间: %d", newToken.ExpiresAt)
	return newToken.Token
}

// isTokenExpiringSoon 检查 token 是否即将过期（60 秒缓冲）
func isTokenExpiringSoon(expiresAt int64) bool {
	return time.Now().Unix() >= expiresAt-tokenRefreshBufferSeconds
}

// copilotTokenAPIResponse Copilot Token API 的响应格式
type copilotTokenAPIResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

// fetchNewCopilotToken 使用 GitHub OAuth token 获取新的 Copilot token
func fetchNewCopilotToken(domain, githubToken string) (*copilotTokenAPIResponse, error) {
	url := CopilotTokenURL(domain)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Authorization", "token "+githubToken)
	req.Header.Set("User-Agent", CopilotUserAgent)
	req.Header.Set("Editor-Version", CopilotEditorVersion)
	req.Header.Set("Editor-Plugin-Version", CopilotPluginVersion)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("GitHub token 无效或已过期")
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("未订阅 Copilot")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result copilotTokenAPIResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if result.Token == "" {
		return nil, fmt.Errorf("响应中 token 为空")
	}
	return &result, nil
}
