package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"switchai/config"
	"switchai/proxy"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ============================================================
// GitHub Copilot OAuth Device Code Flow API
// 端口自 cc-switch: src-tauri/src/commands/copilot.rs
// ============================================================

// CopilotOAuthDeviceFlowResponse GitHub OAuth 设备码流程响应
type CopilotOAuthDeviceFlowResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// CopilotOAuthTokenResponse OAuth token 响应
type CopilotOAuthTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
}

// CopilotTokenResponse Copilot token 响应
type CopilotTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
	Type      string `json:"type"`
}

// GitHubUserInfo GitHub 用户信息
type GitHubUserInfo struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
}

// CopilotAuthStatus 认证状态
type CopilotAuthStatus struct {
	HasAccounts bool     `json:"has_accounts"`
	Accounts    []Account `json:"accounts"`
}

// Account 账号信息（不含 token）
type Account struct {
	ID           string `json:"id"`
	GitHubDomain string `json:"github_domain"`
	UserID       int64  `json:"user_id"`
	AccountID    string `json:"account_id"`
	Login        string `json:"login"`
	AvatarURL    string `json:"avatar_url"`
	IsDefault    bool   `json:"is_default"`
	CreatedAt    string `json:"created_at"`
}

// ============================================================
// API Handlers
// ============================================================

// startCopilotDeviceFlow POST /api/copilot/device-flow
func startCopilotDeviceFlow(c *gin.Context) {
	var req struct {
		GitHubDomain string `json:"github_domain"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	domain := req.GitHubDomain
	if domain == "" {
		domain = "github.com"
	}

	normalized, err := proxy.NormalizeGitHubDomain(domain)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid domain: %v", err)})
		return
	}

	clientID := proxy.GithubClientID(normalized)
	deviceCodeURL := proxy.GithubDeviceCodeURL(normalized)

	body := fmt.Sprintf("client_id=%s&scope=copilot", clientID)
	httpReq, _ := http.NewRequest("POST", deviceCodeURL, bytes.NewBufferString(body))
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		log.Printf("startCopilotDeviceFlow: request failed: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("request failed: %v", err)})
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		log.Printf("startCopilotDeviceFlow: status %d, body: %s", resp.StatusCode, string(respBody))
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("GitHub returned status %d", resp.StatusCode)})
		return
	}

	var result CopilotOAuthDeviceFlowResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to parse response"})
		return
	}

	// 默认 interval 为 5，但 GitHub 建议 interval + 3
	if result.Interval < 5 {
		result.Interval = 5
	}

	c.JSON(http.StatusOK, gin.H{
		"device_code":       result.DeviceCode,
		"user_code":         result.UserCode,
		"verification_uri":  result.VerificationURI,
		"expires_in":        result.ExpiresIn,
		"interval":          result.Interval,
		"github_domain":     normalized,
	})
}

// pollCopilotToken POST /api/copilot/poll
func pollCopilotToken(c *gin.Context) {
	var req struct {
		DeviceCode   string `json:"device_code"`
		GitHubDomain string `json:"github_domain"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	domain := req.GitHubDomain
	if domain == "" {
		domain = "github.com"
	}

	normalized, err := proxy.NormalizeGitHubDomain(domain)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid domain: %v", err)})
		return
	}

	clientID := proxy.GithubClientID(normalized)
	tokenURL := proxy.GithubOAuthTokenURL(normalized)

	body := fmt.Sprintf("client_id=%s&device_code=%s&grant_type=urn:ietf:params:oauth:grant-type:device_code",
		clientID, req.DeviceCode)
	httpReq, _ := http.NewRequest("POST", tokenURL, bytes.NewBufferString(body))
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("request failed: %v", err)})
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	var tokenResp map[string]interface{}
	json.Unmarshal(respBody, &tokenResp)

	// 检查错误类型
	if errStr, ok := tokenResp["error"].(string); ok {
		if errStr == "authorization_pending" {
			c.JSON(http.StatusOK, gin.H{"status": "pending"})
			return
		}
		if errStr == "slow_down" {
			interval := 5
			if i, ok := tokenResp["interval"].(float64); ok {
				interval = int(i) + 3
			}
			c.JSON(http.StatusOK, gin.H{"status": "pending", "interval": interval})
			return
		}
		// 其他错误
		c.JSON(http.StatusBadGateway, gin.H{"status": "error", "error": errStr})
		return
	}

	githubToken := tokenResp["access_token"].(string)
	tokenType := "Bearer"
	if tt, ok := tokenResp["token_type"].(string); ok {
		tokenType = tt
	}

	// 获取 GitHub 用户信息
	userInfo, err := fetchGitHubUserInfo(normalized, githubToken)
	if err != nil {
		log.Printf("pollCopilotToken: fetchGitHubUserInfo failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "failed to fetch user info"})
		return
	}

	// 获取 Copilot token
	copilotResp, err := fetchCopilotToken(normalized, githubToken)
	if err != nil {
		log.Printf("pollCopilotToken: fetchCopilotToken failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "failed to fetch copilot token"})
		return
	}

	// 保存 token
	accountID := proxy.CompositeAccountID(normalized, strconv.FormatInt(userInfo.ID, 10))
	now := time.Now().Format(time.RFC3339)

	tokenRecord := &config.CopilotToken{
		ID:           uuid.New().String(),
		GitHubDomain: normalized,
		UserID:       userInfo.ID,
		AccountID:    accountID,
		GitHubToken:  githubToken,
		CopilotToken: copilotResp.Token,
		TokenType:    tokenType,
		ExpiresAt:    copilotResp.ExpiresAt,
		Login:        userInfo.Login,
		AvatarURL:    userInfo.AvatarURL,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	store := config.GetCopilotTokenStore()
	if err := store.SaveCopilotToken(tokenRecord); err != nil {
		log.Printf("pollCopilotToken: SaveCopilotToken failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "failed to save token"})
		return
	}

	// 如果是第一个账号，设为默认
	accounts, _ := store.ListCopilotAccounts()
	if len(accounts) == 1 {
		store.SetDefaultAccountID(accountID)
	}

	c.JSON(http.StatusOK, gin.H{
		"status":      "success",
		"account_id":  accountID,
		"login":       userInfo.Login,
		"avatar_url":  userInfo.AvatarURL,
		"github_domain": normalized,
	})
}

// getCopilotAccounts GET /api/copilot/accounts
func getCopilotAccounts(c *gin.Context) {
	store := config.GetCopilotTokenStore()
	accounts, err := store.ListCopilotAccounts()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"accounts": accounts})
}

// removeCopilotAccount DELETE /api/copilot/accounts/:id
func removeCopilotAccount(c *gin.Context) {
	accountID := c.Param("id")
	if accountID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account_id is required"})
		return
	}

	store := config.GetCopilotTokenStore()
	if err := store.DeleteCopilotTokenByAccountID(accountID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 如果删除的是默认账号，重置默认
	defaultID, _ := store.GetDefaultAccountID()
	if defaultID == accountID {
		store.SetDefaultAccountID("")
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// setDefaultCopilotAccount POST /api/copilot/accounts/:id/default
func setDefaultCopilotAccount(c *gin.Context) {
	accountID := c.Param("id")
	if accountID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account_id is required"})
		return
	}

	store := config.GetCopilotTokenStore()
	if err := store.SetDefaultAccountID(accountID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// copilotLogout POST /api/copilot/logout
func copilotLogout(c *gin.Context) {
	store := config.GetCopilotTokenStore()
	if err := store.DeleteAllCopilotTokens(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// getCopilotAuthStatus GET /api/copilot/status
func getCopilotAuthStatus(c *gin.Context) {
	store := config.GetCopilotTokenStore()
	accounts, err := store.ListCopilotAccounts()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"has_accounts": len(accounts) > 0,
		"accounts":      accounts,
	})
}

// ============================================================
// Helper Functions
// ============================================================

func fetchGitHubUserInfo(domain, token string) (*GitHubUserInfo, error) {
	url := proxy.GithubUserURL(domain)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var user GitHubUserInfo
	if err := json.Unmarshal(body, &user); err != nil {
		return nil, err
	}
	return &user, nil
}

func fetchCopilotToken(domain, githubToken string) (*CopilotTokenResponse, error) {
	url := proxy.CopilotTokenURL(domain)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("Editor-Version", proxy.CopilotEditorVersion)
	req.Header.Set("Editor-Plugin-Version", proxy.CopilotPluginVersion)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	body, _ := io.ReadAll(resp.Body)
	var result CopilotTokenResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
