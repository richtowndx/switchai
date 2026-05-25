# SwitchAI - 智能 API 聚合代理服务

![Go Version](https://img.shields.io/badge/Go-1.21+-blue) ![License](https://img.shields.io/badge/License-MIT-green)

SwitchAI 是一个功能强大的本地 API 聚合代理服务，为 **Claude Code**、**OpenAI SDK**、**Copilot Chat** 等客户端提供统一的 AI 接口网关。支持 **Anthropic**、**OpenAI**、**GitHub Copilot** 等多协议自动适配和智能路由，内置 Web 管理界面、TOTP 双因素认证、密钥配额管理、实时统计监控等企业级特性。

---

## 📦 特性一览

### 🔄 统一网关路由
| 客户端请求格式 | 支持的路由端点 | 可转发的上游格式 |
|---------------|---------------|-----------------|
| Anthropic Messages API | `POST /v1/messages` | Anthropic / OpenAI |
| OpenAI Chat Completions | `POST /v1/chat/completions` | OpenAI / Anthropic |
| OpenAI Completions | `POST /v1/completions` | OpenAI / Anthropic |
| OpenAI Responses API (Codex) | `POST /responses`、`POST /v1/responses` | OpenAI / Anthropic |
| GitHub Copilot Chat | `POST /chat/completions`、`/copilot/*` | Copilot |

---

## ✨ 核心功能

### 🏢 多提供商管理
- 支持 **Anthropic**、**OpenAI**、**GitHub Copilot** 等多种 AI 服务商
- **基于 FNV-1a 哈希的客户端亲和性路由**：同一客户端 IP:Port 始终路由到同一提供商，维持会话连续性
- **Round-Robin 轮询**：格式匹配的提供商间自动负载均衡
- **故障自动切换**：上游返回 5xx/429/529 时自动切换到备用提供商
- **Web 界面热切换**：一键激活/切换活跃提供商，无需重启服务
- **5 类模型映射**：每个提供商独立配置 `default`/`haiku`/`sonnet`/`opus`/`fast` 模型映射

### 🔄 智能协议转换
- **Anthropic ↔ OpenAI 双向自动转换**：请求体、响应体、SSE 流式事件全链路转换
- **tool_calls ↔ tool_use 转换**：OpenAI function calling 与 Anthropic tool_use 自动互转
- **tool ↔ tool_result 转换**：工具调用结果格式自动适配
- **SSE 流式实时转换**：`message_start`/`content_block_delta`/`message_delta` 等事件的实时转换
- **系统消息智能处理**：自动避免破坏 tool_calls 消息序列
- **图片格式转换**：Anthropic `image` block ↔ OpenAI `image_url` block
- **thinking/redacted_thinking 过滤**：Anthropic 专属 block 在转 OpenAI 时自动过滤

### 🛠️ Codex / Responses API 支持
原生支持 [OpenAI Responses API](https://platform.openai.com/docs/api-reference/responses) 格式：
- **请求转换**：`instructions`/`input` → Chat Completions `messages`
- **响应转换**：Chat Completions `choices` → Responses API `output`
- **流式 SSE 转换**：Chat SSE → Responses API SSE（`response.created`、`response.output_text.delta`、`response.completed` 等事件）
- **function_call 序列合并**：将 Responses API 交错的 `function_call`/`message` 合并为正确的 Chat 消息序列
- **函数调用参数增量合并**：跨多个 SSE chunk 的增量拼合
- **模型智能映射**：`gpt-5.2-codex` → `sonnet_model`、`glm-5` → `haiku_model` 等

### 🅸 GitHub Copilot 集成
- **OAuth 设备码流程**：Web 界面一键启动 Copilot 认证
- **GitHub.com 和 GHES 双支持**：支持 GitHub Enterprise Server 私有部署
- **Token 自动刷新**：过期前 60 秒自动刷新，无需手动干预
- **多账号管理**：支持添加多个 Copilot 账号，设置默认账号
- **模型 ID 归一化**：自动处理 `claude-sonnet-4-6[1m]` → `claude-sonnet-4.6` 等格式变换
- **兼容性回退**：非 Chat 模型（如 Codex 模型）自动回退到 `gpt-4o`
- **必要 Header 注入**：自动注入 `Editor-Version`、`Copilot-Integration-Id` 等必需头

### 🔐 安全与认证
- **TOTP 双因素认证**：基于 RFC 6238，兼容 Google/Microsoft Authenticator
- **API 密钥管理**：生成 `sk-` 开头密钥，支持备注标识、启用/禁用
- **四维配额控制**：
  - 每日请求次数限额
  - 总请求次数限额
  - 每日花费限额（美元）
  - 总花费限额（美元）
- **多端会话**：Cookie 会话管理，支持多端同时登录
- **内网跳过认证**：`-skip` 参数部署内网环境
- **HTTPS 支持**：内置自签 CA，自动生成 TLS 证书

### 📊 统计与监控
- **SQLite 持久化统计**：Provider 维度 + 密钥维度的使用统计
- **实时数据**：通过 WebSocket 推送实时统计更新
- **密钥访问审计**：记录每个密钥的请求 IP、Token 用量、花费
- **日统计聚合**：`key_daily_stats` 表聚合每日数据
- **首 Token 耗时记录**：`time_to_first_ms` 指标
- **缓存 Token 统计**：`cache_read_input_tokens` 跟踪提示词缓存效率
- **零 Token 告警**：自动检测和记录 input/output tokens 为 0 的异常请求

### 📜 请求历史
- **SQLite 持久化**：存储所有请求/响应记录
- **分页浏览**：支持页码和每页条数自定义
- **毫秒级时间戳**：精确到毫秒的时间显示
- **详情查看**：展示完整请求/响应体
- **JSON 格式化**：一键格式化 JSON 请求/响应
- **WebSocket 实时推送**：新记录实时推送到前端

### 🌐 Web 管理界面
基于 Go embed 嵌入的完整前端页面：
- **仪表盘**：提供商概览、状态展示
- **提供商管理**：添加/编辑/删除/测试/激活提供商
- **模型映射配置**：5 类模型独立配置
- **代理设置**：每个提供商独立配置 HTTP/SOCKS5 代理
- **密钥管理**：生成/编辑/限额配置/状态切换
- **实时统计**：Provider 和密钥维度的使用统计
- **请求历史**：分页浏览和详情查看
- **Copilot 认证**：设备码流程、账号管理

### 🔌 连接管理
- **TCP 连接级代理复用**：同一 TCP 连接复用同一个 CcProxy 实例
- **连接跟踪**：实时跟踪连接流量（BytesRead/BytesWrite）
- **空闲清理**：30 秒周期性检查，5 分钟无活动自动清理
- **并发安全**：基于 `sync.Map` 的并发原语，支持高并发
- **连接统计**：活动连接数、总连接数、流量统计

### 📋 日志系统
- **按小时轮转**：日志文件按日期+小时命名
- **分级日志**：`info`/`error` 分离存储
- **自动清理**：保留 3 天日志文件，超期自动清理
- **Gin 中间件集成**：HTTP 请求日志自动记录

---

## 🚀 快速开始

### 开发模式

```bash
# 克隆仓库
git clone <repository-url>
cd switchai

# 安装依赖
go mod tidy

# 运行开发服务器（默认端口 7777）
go run main.go

# 指定端口运行
go run main.go -p 8080

# 跳过认证（内网开发）
go run main.go -p 8080 -skip
```

### 生产部署

```bash
# 构建所有平台版本
./build.sh               # Linux/macOS
./build.bat              # Windows

# 输出文件在 dist/ 目录:
#   dist/switchai-windows-amd64.exe
#   dist/switchai-linux-amd64
#   dist/switchai-linux-aarch64

# 启动服务
./switchai-linux-amd64 -p 7777
```

### 系统服务安装

```bash
# Windows（需要管理员权限）
switchai-windows-amd64.exe -install
sc start SwitchAI

# Linux（需要 root 权限）
sudo ./switchai-linux-amd64 -install
sudo systemctl start switchai
```

---

## ⚙️ 配置指南

### 命令行参数

| 参数 | 说明 | 示例 |
|------|------|------|
| `-p` | 监听端口（默认 7777） | `-p 8080` |
| `-install` | 安装为系统服务 | `-install` |
| `-uninstall` | 卸载系统服务 | `-uninstall` |
| `-skip` | 跳过认证（内网模式） | `-skip` |
| `-reset` | 重置 2FA 认证 | `-reset` |
| `-tls` | 启用 HTTPS | `-tls` |

### 提供商配置

每个提供商支持：

| 字段 | 说明 | 示例 |
|------|------|------|
| `name` | 提供商名称 | `Anthropic Official` |
| `base_url` | API 基础地址 | `https://api.anthropic.com` |
| `api_key` | API 密钥 | `sk-ant-...` |
| `type` | 提供商类型（Anthropic / OpenAI / Copilot） | `anthropic` |
| `default_model` | 默认模型 | `claude-sonnet-4-6` |
| `haiku_model` | Haiku 模型 | `claude-haiku-4-5` |
| `sonnet_model` | Sonnet 模型 | `claude-sonnet-4-6` |
| `opus_model` | Opus 模型 | `claude-opus-4-7` |
| `fast_model` | 快速模型 | `claude-haiku-4-5` |
| `proxy_url` | 代理地址（可选） | `http://127.0.0.1:7890` |

### Copilot 提供商配置

1. 在 Web 界面点击 **Copilot 认证**
2. 选择 GitHub 部署类型（GitHub.com 或 Enterprise Server）
3. 输入 Enterprise URL（如适用），点击获取验证码
4. 在浏览器中打开 `github.com/login/device` 并输入验证码
5. 授权后在提供商管理中创建 **Copilot** 类型提供商，选择已认证账号

### AI 代理路由映射

```
客户端 (Anthropic SDK)  →  POST /v1/messages        →  SwitchAI
客户端 (OpenAI SDK)     →  POST /v1/chat/completions  →  SwitchAI
客户端 (Responses API)  →  POST /v1/responses         →  SwitchAI
Claude Code (CLI)       →  POST /v1/messages          →  SwitchAI
Copilot (VS Code)       →  POST /chat/completions     →  SwitchAI
```

SwitchAI 自动根据提供商配置决定转发到 **Anthropic API**（`/v1/messages`）还是 **OpenAI API**（`/v1/chat/completions`）。

---

## 📡 API 端点

### Web 管理 API

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/api/login` | TOTP 登录 |
| `POST` | `/api/logout` | 退出登录 |
| `GET` | `/api/totp/status` | 获取 TOTP 状态 |
| `POST` | `/api/totp/setup` | 首次设置 TOTP |
| `POST` | `/api/totp/verify` | 验证并启用 TOTP |
| `GET` | `/api/providers` | 获取所有提供商 |
| `POST` | `/api/providers` | 添加提供商 |
| `PUT` | `/api/providers/:id` | 更新提供商 |
| `DELETE` | `/api/providers/:id` | 删除提供商 |
| `POST` | `/api/providers/:id/activate` | 激活提供商 |
| `POST` | `/api/providers/:id/test` | 测试提供商连接 |
| `GET` | `/api/server-keys` | 获取所有密钥 |
| `POST` | `/api/server-keys` | 添加密钥 |
| `POST` | `/api/server-keys/generate` | 生成新密钥 |
| `PUT` | `/api/server-keys/:id` | 更新密钥 |
| `DELETE` | `/api/server-keys/:id` | 删除密钥 |
| `GET` | `/api/server-keys/:id/stats` | 获取密钥统计 |
| `GET` | `/api/stats` | 获取总体统计 |
| `GET` | `/api/stats/daily` | 获取每日统计 |
| `POST` | `/api/stats/reset` | 重置所有统计 |
| `GET` | `/api/history` | 获取请求历史 |
| `GET` | `/api/history/:id` | 获取请求详情 |
| `GET` | `/api/ws` | WebSocket 实时统计 |
| `GET` | `/api/ws/history` | WebSocket 历史推送 |

### Copilot 管理 API

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/api/copilot/device-flow` | 启动 OAuth 设备码流程 |
| `POST` | `/api/copilot/poll` | 轮询获取 Token |
| `GET` | `/api/copilot/accounts` | 获取已认证账号 |
| `DELETE` | `/api/copilot/accounts/:id` | 移除账号 |
| `POST` | `/api/copilot/accounts/:id/default` | 设置默认账号 |
| `POST` | `/api/copilot/logout` | 登出所有 Copilot 账号 |
| `GET` | `/api/copilot/status` | 获取认证状态 |

---

## 🏗️ 项目架构

```
switchai/
├── main.go                    # 程序入口，CLI 参数处理、服务启动
├── go.mod / go.sum            # Go 模块依赖
├── build.sh / build.bat       # 跨平台构建脚本
├──
├── proxy/                     # 代理处理核心
│   ├── proxy.go              # 主代理逻辑：路由注册、格式转换、流式/非流式处理
│   ├── ccproxy.go            # CcProxy 接口定义：统一的代理抽象层
│   ├── ccproxy_handler.go    # Proxy 错误处理、Token 解析、SSE 提取工具
│   ├── anthropic_proxy.go    # Anthropic 协议代理实现
│   ├── openai_proxy.go       # OpenAI 协议代理实现
│   ├── copilot_proxy.go      # Copilot 协议代理实现（SSE 转换）
│   ├── copilot.go            # Copilot 认证、Token 刷新、模型归一化
│   ├── codex.go              # Codex/Responses API ↔ Chat Completions 转换
│   ├── format_helper.go      # 格式转换辅助函数（OpenAI↔Anthropic）
│   ├── providers.go          # 内置提供商参数过滤规则
│   ├── conn_proxy_manager.go # 连接级代理管理器（TCP 连接复用）
│   ├── tracked_conn.go       # 连接跟踪与流量统计
│   └── session.go            # ConnectionTracker 定义
├── config/                    # 配置管理
│   └── config.go             # SQLite 数据模型、CRUD、Provider/Key/Copilot 管理
├── web/                       # Web 服务和 UI
│   ├── web.go                # Web API 路由、认证中间件、TOTP 处理
│   ├── copilot.go            # Copilot OAuth 设备码流程
│   └── static/               # 前端静态资源（Go embed）
├── stats/                     # 统计模块
│   └── stats.go              # SQLite 持久化统计、WebSocket 广播、配额检查
├── history/                   # 请求历史
│   └── history.go            # SQLite 存储、WebSocket 推送、分页查询
├── logger/                    # 日志系统
│   ├── logger.go             # 分级日志、按小时轮转、自动清理
│   └── middleware.go         # Gin HTTP 日志中间件
├── cert/                      # 证书管理
│   └── cert.go               # 自签 CA 和 TLS 证书生成
├── service/                   # 系统服务
│   └── service.go            # Windows 服务 / Linux systemd 安装管理
├── appdata/                   # 应用数据
│   └── appdata.go            # 数据目录初始化和路径管理
└── socktest/                  # 测试工具
    ├── client/                # TCP 连接测试客户端
    ├── serv/                  # TCP 测试服务端
    └── reqformat/             # 请求格式测试
```

### 关键数据流

```
┌──────────┐   ┌────────────┐   ┌──────────┐   ┌────────────┐   ┌──────────┐
│ 客户端    │ → │ 认证中间件   │ → │ 格式检测  │ → │ 提供商选择   │ → │ 格式转换  │
│(SDK/CLI) │   │(密钥验证/   │   │(Anthropic│   │(哈希路由/   │   │(请求适配) │
│          │   │ 限额检查)   │   │ OpenAI/  │   │ 轮询/故障  │   │          │
└──────────┘   └────────────┘   │ Copilot) │   │ 切换)      │   └────┬─────┘
                                └──────────┘   └────────────┘        │
                                                                      ▼
┌──────────┐   ┌────────────┐   ┌──────────┐   ┌────────────┐   ┌──────────┐
│ 客户端    │ ← │ 响应转换    │ ← │ 上游 API  │ ← │ 请求发送   │ ← │ 模型映射  │
│(流式转发) │   │(SSE/非流式) │   │(提供商的  │   │(代理/直连) │   │(模型名    │
│          │   │            │   │ 后端)     │   │            │   │ 解析)     │
└──────────┘   └────────────┘   └──────────┘   └────────────┘   └──────────┘
```

---

## 📊 数据存储

### 数据库结构

| 文件 | 用途 | 表 |
|------|------|----|
| `config.db` | 配置数据库 | `config`、`providers`、`server_keys`、`copilot_tokens`、`copilot_default_account` |
| `stats.db` | 统计数据库 | `usage_records`、`provider_stats`、`key_stats`、`key_daily_stats` |
| `history.db` | 历史数据库 | `request_records` |

### 文件布局

```
.switchai/
├── config.db              # 配置数据（提供商、密钥、Copilot Token、会话等）
├── stats.db               # 统计数据（使用记录、Provider/Key 统计）
├── history.db             # 请求历史记录
├── certs/                 # TLS 证书目录
│   ├── ca.pem            # 自签 CA 根证书
│   ├── server.pem        # 服务端证书
│   └── server-key.pem    # 服务端私钥
└── logs/                  # 日志文件
    ├── info_2026-05-26_15.log
    └── error_2026-05-26_15.log
```

### 模型映射表

| 配置键 | 说明 | 客户端传入示例 |
|--------|------|---------------|
| `default_model` | 默认模型（兜底） | `claude-sonnet-4-6` |
| `haiku_model` | Haiku 模型 | `claude-haiku-4-5` |
| `sonnet_model` | Sonnet 模型 | `claude-sonnet-4-6` |
| `opus_model` | Opus 模型 | `claude-opus-4-7` |
| `fast_model` | 快速模型 | `claude-haiku-4-5` |

客户端传入模型名 → 查表映射 → 兜底 `default_model` → 兼容旧 `model` 字段。

---

## 🧪 测试

```bash
# 运行所有测试
go test ./...

# 代理模块测试
go test ./proxy/...

# 配置模块测试
go test ./config/...

# 格式转换测试
go test ./proxy/ -run TestFormatConversion

# Codex 转换测试
go test ./proxy/ -run TestCodexConversion

# 内容块过滤测试
go test ./proxy/ -run TestFilterUnsupportedContentBlocks

# Copilot 模型归一化测试
go test ./proxy/ -run TestNormalizeCopilotModelID
```

---

## 🛠️ 高级用法

### 格式转换示例

**OpenAI → Anthropic（请求转换）**
```json
// 客户端发送 OpenAI 格式
POST /v1/chat/completions
{
  "model": "gpt-4",
  "messages": [{"role": "user", "content": "Hello"}],
  "stream": true
}

// SwitchAI 转换为 Anthropic 格式发送到上游
POST /v1/messages
{
  "model": "claude-sonnet-4-6",
  "messages": [{"role": "user", "content": "Hello"}],
  "max_tokens": 4096,
  "stream": true
}
```

**tool_calls 转换**
```json
// OpenAI tool_calls → Anthropic tool_use
{
  "role": "assistant",
  "tool_calls": [{
    "id": "call_123",
    "type": "function",
    "function": {
      "name": "search",
      "arguments": "{\"query\":\"test\"}"
    }
  }]
}

// 自动转换为
{
  "role": "assistant",
  "content": [{
    "type": "tool_use",
    "id": "call_123",
    "name": "search",
    "input": {"query": "test"}
  }]
}
```

**SSE 流式实时转换**
```
# OpenAI SSE 事件流
原始: data: {"id":"...","choices":[{"delta":{"content":"Hello"}}]}
转换: event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

# Anthropic SSE 事件流
原始: event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}
转换: data: {"id":"...","choices":[{"index":0,"delta":{"content":"Hello"}}]}
```

### 提供商参数过滤

部分第三方 API 不支持某些参数，SwitchAI 自动按 Hostname 过滤：

| 提供商 Hostname | 过滤的参数 |
|----------------|------------|
| `api.minimaxi.com` | `output_config` |
| `api.stepfun.com` | `output_config` |
| `aigw-gzgy2.cucloud.cn` | `output_config` |
| 其他 | `output_config`（OpenAI 格式时）|

### 内容块过滤

格式转换时自动过滤不兼容的内容块类型：

| 转换方向 | 过滤的块类型 | 保留的块类型 |
|---------|-------------|-------------|
| Anthropic → OpenAI | `thinking`、`redacted_thinking` | `text`、`image_url` |
| OpenAI → Anthropic | 无 | 全部保留 |

---

## 🔧 故障排查

| 问题 | 可能原因 | 解决方法 |
|------|---------|---------|
| 401 Unauthorized | API Key 无效或密钥已禁用 | 检查 Web 界面密钥状态，重新生成 |
| 403 Forbidden | 超出密钥限额 | 检查 `key_daily_stats`，调整限额或重置 |
| 上游返回 5xx | 上游服务故障 | 配置备用提供商，启用故障切换 |
| 格式转换错误 | 请求格式不匹配 | 确认提供商类型与客户端格式匹配 |
| SSE 流式中断 | 事件边界处理不当 | 检查日志中的 stream 扫描错误 |
| Copilot 认证失败 | Token 过期或未订阅 | 重新执行设备码流程，检查 Copilot 订阅 |
| TOTP 验证错误 | 系统时间不同步 | 校准系统时间，检查 NTP 服务 |

```bash
# 查看实时日志
tail -f ~/.switchai/logs/info_*.log

# 查看错误日志
tail -f ~/.switchai/logs/error_*.log

# 搜索特定关键词
grep "error\|fail\|warn" ~/.switchai/logs/info_*.log
```

---

## 🔒 VSCode Claude Code 配置

在 VSCode 的 `settings.json` 中配置：

```json
{
  "claudeCode.environmentVariables": [
    {
      "name": "ANTHROPIC_AUTH_TOKEN",
      "value": "sk-xxxxxxxxx"
    },
    {
      "name": "ANTHROPIC_BASE_URL",
      "value": "http://localhost:7777"
    },
    {
      "name": "ANTHROPIC_MODEL",
      "value": "claude-sonnet-4-6"
    }
  ]
}
```

- `ANTHROPIC_AUTH_TOKEN`：Web 管理页面生成的 `sk-` 开头密钥
- `ANTHROPIC_BASE_URL`：SwitchAI 服务地址
- `ANTHROPIC_MODEL`：会自动匹配提供商配置的模型映射表

---

## 🔨 构建说明

### 依赖

- Go 1.21+
- GCC/LLVM（SQLite 需要 CGO，但可通过 `modernc.org/sqlite` 实现纯 Go 构建）

### 构建命令

```bash
# 本地构建
go build -o switchai .

# 跨平台构建（Linux amd64 + arm64 + Windows）
./build.sh

# 构建并注入版本信息
go build -ldflags="-s -w -X main.versionMajor=1 -X main.versionMinor=0 -X main.versionPatch=0" -o switchai .
```

---

## 🤝 贡献指南

1. Fork 本仓库
2. 创建特性分支 (`git checkout -b feature/AmazingFeature`)
3. 提交更改 (`git commit -m 'Add some AmazingFeature'`)
4. 推送到分支 (`git push origin feature/AmazingFeature`)
5. 开启 Pull Request

---

## 📄 许可证

本项目采用 MIT 许可证 - 详见 [LICENSE](LICENSE) 文件

## 🙏 致谢

- [Gin Web Framework](https://github.com/gin-gonic/gin) - 高性能 Go Web 框架
- [Gorilla WebSocket](https://github.com/gorilla/websocket) - WebSocket 支持
- [modernc.org/sqlite](https://modernc.org/sqlite) - 纯 Go SQLite 驱动
- [Anthropic Claude API](https://www.anthropic.com/) - AI 服务提供商
- [OpenAI API](https://openai.com/) - AI 服务提供商
- [GitHub Copilot](https://github.com/features/copilot) - AI 编程助手
- [Brotli](https://github.com/andybalholm/brotli) - Brotli 压缩支持

---

> **注意**：本工具仅供学习和开发使用，请遵守相关 AI 服务商的使用条款和 API 使用政策。
