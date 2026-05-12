# SwitchAI - Claude API 聚合服务

一个本地 Claude API 聚合服务，可以管理多个 Claude API 提供商，并提供统一的接口给 VSCode Claude Code 使用。

## 功能特性

- 🔄 **多提供商管理**：支持配置多个 Claude API 提供商（不同的 BaseURL 和 API Key）
- 🎯 **一键切换**：通过 Web 界面快速切换当前使用的提供商
- 🔁 **轮询重试**：自动在最多 3 个备用提供商之间轮询重试，提升服务可用性
- 🤖 **多模型配置**：每个提供商可独立配置 default / haiku / sonnet / opus / fast 五类模型
- 🔄 **格式自动转换**：支持 Anthropic 与 OpenAI API 格式互转，统一透明代理
- 📊 **Token 统计**：实时显示 Token 使用情况（输入/输出/总计）
- 📜 **请求历史**：记录最近1000条请求，支持分页查看和详细内容展示
- 🔑 **密钥维度统计**：按 API 密钥维度统计请求数、Token 用量和花费
- ⚖️ **密钥限额控制**：支持每日/总计请求次数限额和花费限额
- 📝 **日志轮转**：按日期+时间自动轮转日志文件，跨天自动切换
- 🌐 **Web 管理界面**：简洁美观的管理界面
- 🚀 **透明代理**：自动转发 Claude API 请求到当前选中的提供商
- 🔐 **2FA 登录**：TOTP 双因素认证，支持多端同时登录
- 💾 **服务安装**：支持安装为 Windows/Linux 系统服务
- 🔒 **HTTPS 支持**：内置自签 CA 自动生成 TLS 证书，一键启用 HTTPS
- 💿 **SQLite 持久化**：配置数据存储于 SQLite 数据库，支持在线 schema 迁移

## 快速开始


---

## 界面
![主界面](pic/01.png)

![消耗界面](pic/02.png)
![日志详细](pic/03.png)

### 开发模式

```bash
# 安装依赖
go mod tidy

# 直接运行
go run main.go

# 指定端口运行
go run main.go -port 8080
```

### 生产部署

```bash
# 构建所有平台版本
build.bat

# 输出文件在 dist/ 目录:
# - switchai-windows-amd64.exe (web资源已内嵌)
# - switchai-linux-amd64 (web资源已内嵌)
```

**注意**: Web静态资源已通过Go embed打包进二进制文件，无需单独部署web目录。

## 命令行参数

```bash
# 默认端口(7777)启动，首次访问会显示2FA设置页面
switchai-windows-amd64.exe

# 指定端口启动
switchai-windows-amd64.exe -p 8080

# 启用 HTTPS（自动生成 TLS 证书，首次运行后位于 .switchai/certs/）
switchai-windows-amd64.exe -tls

# 指定端口并启用 HTTPS
switchai-windows-amd64.exe -p 443 -tls

# 安装为系统服务
switchai-windows-amd64.exe -install

# 安装为系统服务并指定端口
switchai-windows-amd64.exe -install -p 8080

# 安装为系统服务并启用 HTTPS
switchai-windows-amd64.exe -install -tls

# 卸载系统服务
switchai-windows-amd64.exe -uninstall

# 跳过认证模式（内网部署，无需密钥密码）
switchai-windows-amd64.exe -skip

# 跳过认证并指定端口
switchai-windows-amd64.exe -p 8080 -skip

# 重置 2FA（清除 TOTP 密钥，访问页面将跳转首次绑定）
switchai-windows-amd64.exe -reset
```

**首次启动**: 首次访问时，会显示 2FA 二维码，绑定 authenticator 应用后使用生成的 6 位验证码登录。若忘记密钥，删除配置文件重新运行，或使用 `-reset` 参数重置。

**HTTPS 证书**: 使用 `-tls` 参数启动时，会在 `.switchai/certs/` 目录自动生成自签 CA 根证书 (`ca.pem`) 和服务端证书 (`server.pem`/`server-key.pem`)。将 `ca.pem` 导入系统受信任根证书存储后浏览器不再告警。

**内网部署**: 使用 `-skip` 参数启动时，全程不需要密钥密码即可使用，适合内网部署场景。

## 服务安装

### Windows

服务安装路径: `C:\Program Files\SwitchAI`

```bash
# 安装服务 (需要管理员权限)
switchai-windows-amd64.exe -install

# 服务管理命令
sc start SwitchAI
sc stop SwitchAI
sc query SwitchAI

# 卸载服务 (保留数据文件)
switchai-windows-amd64.exe -uninstall
```

**注意**: 卸载服务时会保留配置文件、历史记录和日志文件，只删除二进制程序。

### Linux

服务安装路径: `/usr/local/bin`，配置文件: `~/.switchai/`

```bash
# 安装服务 (需要 root 权限)
sudo ./switchai-linux-amd64 -install

# 安装服务并启用 HTTPS
sudo ./switchai-linux-amd64 -install -tls

# 服务管理命令
sudo systemctl start switchai
sudo systemctl stop switchai
sudo systemctl status switchai
sudo systemctl enable switchai  # 开机自启

# 卸载服务 (保留数据文件)
sudo ./switchai-linux-amd64 -uninstall
```

**注意**: 卸载服务时会保留配置文件、历史记录和日志文件，只删除二进制程序和systemd服务文件。

## Web 界面

访问地址: `http://localhost:7777`

### 页面说明

1. **首页** (`/`) - 提供商管理和概览
2. **Token 统计** (`/log.html`) - 实时 Token 使用统计
3. **请求历史** (`/history.html`) - 详细的请求/响应历史记录

### 请求历史功能

- **分页浏览**：最近1000条请求，每页20条
- **毫秒级时间戳**：精确到毫秒的时间显示 `2026-03-16 18:51:50.666`
- **详情查看**：点击"View Details"查看完整请求/响应
- **JSON 格式化**：一键格式化 JSON 请求/响应体
- **持久化存储**：所有历史记录保存到 `history.json`

## 日志系统

### 日志文件

日志存储在 `logs/` 目录，按日期时间轮转:

- `info_2026-03-16_18-51-50.log` - 信息日志
- `error_2026-03-16_18-51-50.log` - 错误日志

### 日志轮转

- **自动轮转**：每天午夜自动切换新日志文件
- **文件命名**：`info_YYYY-MM-DD_HH-MM-SS.log`
- **保留策略**：日志永久保留（需手动清理）

## 配置 VSCode Claude Code

在 VSCode 的 `settings.json` 中配置：

```json
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
```

**说明**：
- `ANTHROPIC_AUTH_TOKEN` 填写在 Web 管理页面生成的 `sk-` 开头的密钥
- `ANTHROPIC_BASE_URL` 填写服务地址，启用 HTTPS 后改为 `https://localhost:7777`
- `ANTHROPIC_MODEL` 会自动匹配提供商配置的模型映射表

## API 端点

### 认证

- `POST /api/login` - TOTP 登录
- `POST /api/logout` - 退出登录

### 2FA

- `POST /api/totp/setup` - 首次设置 TOTP
- `POST /api/totp/verify` - 验证 TOTP
- `GET /api/totp/status` - 获取 TOTP 状态

### 提供商管理

- `GET /api/providers` - 获取所有提供商
- `POST /api/providers` - 添加提供商
- `PUT /api/providers/:id` - 更新提供商
- `DELETE /api/providers/:id` - 删除提供商
- `POST /api/providers/:id/activate` - 激活提供商
- `POST /api/providers/:id/test` - 测试提供商连接

### 服务器密钥管理

- `GET /api/server-keys` - 获取所有服务器密钥
- `POST /api/server-keys` - 添加服务器密钥
- `PUT /api/server-keys/:id` - 更新服务器密钥
- `DELETE /api/server-keys/:id` - 删除服务器密钥
- `POST /api/server-keys/generate` - 生成新密钥
- `GET /api/server-keys/:id/stats` - 获取密钥统计
- `POST /api/server-keys/:id/test` - 测试密钥连接

### 统计信息

- `GET /api/stats` - 获取 Token 统计
- `GET /api/stats/daily` - 获取每日统计
- `POST /api/stats/reset` - 重置所有统计
- `POST /api/stats/reset/:provider_id` - 重置指定提供商统计

### 请求历史

- `GET /api/history?page=1&page_size=20` - 获取分页历史记录
- `GET /api/history/:id` - 获取详细请求记录

### WebSocket

- `GET /api/ws` - 实时统计更新
- `GET /api/ws/history` - 实时历史更新

### Claude API 代理

- `ANY /v1/*` - 代理所有 Claude API 请求

## 数据存储

- `config.db` — 配置数据（SQLite 数据库，包含提供商、服务器密钥、2FA、会话等信息）
- `history.json` — 请求历史记录（最近1000条）
- `logs/` — 应用日志（按日期时间轮转）
- `.switchai/certs/` — TLS 证书（HTTPS 模式下自动生成）

## 提供商模型配置

每个提供商支持配置 **5 类模型**，代理根据请求中的 `model` 字段自动匹配：

| 模型键名 | 说明 | 示例 |
|---------|------|------|
| `default_model` | 默认模型（兜底） | `claude-sonnet-4-6` |
| `haiku_model` | Haiku 模型 | `claude-haiku-4-5-20241022` |
| `sonnet_model` | Sonnet 模型 | `claude-sonnet-4-6` |
| `opus_model` | Opus 模型 | `claude-opus-4-7-20250514` |
| `fast_model` | Fast 模型 | `claude-haiku-4-5-20241022` |

模型匹配规则：请求中传入的 model 名称 → 查表找到对应的实际模型名 → 兜底到 `default_model` → 兼容旧 `model` 字段。

## API 格式自动转换

每个提供商可以独立配置 **API 格式**（`is_openai_format`），代理自动处理格式转换：

- **OpenAI → Anthropic**：OpenAI 格式的 `/v1/chat/completions` 请求自动转为 Anthropic `/v1/messages` 格式
- **Anthropic → OpenAI**：Anthropic 格式的 `/v1/messages` 请求自动转为 OpenAI `/v1/chat/completions` 格式
- **SSE 流式转换**：流式响应（Server-Sent Events）实时双向转换，包括 `message_start`/`content_block_delta`/`message_delta` 等事件映射

## 多提供商轮询重试

当上游提供商返回可重试错误（5xx、429、529）时，自动切换到下一个同格式的提供商重试，最多尝试 **3 个**不同提供商。

## 服务器密钥管理

支持生成和管理 `sk-` 开头的 API 密钥，每个密钥可配置：

- **备注**：标识密钥归属
- **启用/禁用**：随时切换密钥状态
- **每日请求次数限额**：0 表示不限
- **总请求次数限额**：0 表示不限
- **每日花费限额 ($)**：0 表示不限
- **总花费限额 ($)**：0 表示不限

密钥维度的统计信息包括：今日/总请求数、输入/输出/总 Token 数、总花费、访问 IP 列表。

## 项目结构

```
switchai/
├── main.go              # 入口文件，CLI 参数处理
├── build.bat            # Windows 构建脚本
├── build.sh             # Linux/macOS 构建脚本
├── appdata/             # 应用数据目录管理
├── cert/                # TLS 证书自动生成（自签 CA + 服务端证书）
├── config/              # 配置管理（SQLite 数据库）
├── logger/              # 日志系统（日期轮转）
├── history/             # 请求历史追踪
├── proxy/               # API 代理处理
├── stats/               # Token 统计
├── service/             # 服务安装管理
└── web/                 # Web 服务和 API
    ├── web.go           # Web API + 静态资源 embed
    └── static/          # HTML/CSS/JS (打包进二进制)
        ├── index.html   # 首页（提供商管理、密钥管理）
        └── log.html     # Token 统计
```

**特性**: 使用Go embed将web静态资源打包进二进制文件，单文件部署，无需额外依赖。

## 使用场景

1. **多账号管理**：管理多个 Claude API 账号，根据需要切换
2. **负载均衡**：轮询机制在多个 API 提供商之间自动分配请求，避免单一账号限流
3. **成本优化**：根据不同提供商的价格和配额灵活切换，密钥维度限额控制预算
4. **开发测试**：在官方 API 和第三方代理之间快速切换
5. **请求审计**：记录和审查所有 API 请求/响应
6. **多端共享**：生成多个 `sk-` 密钥分发给不同客户端，统一管理
7. **格式适配**：OpenAI 格式的客户端可以无缝访问 Anthropic API，反之亦然

## 系统要求

- Go 1.21 或更高版本
- Windows 10+ 或 Linux (systemd)
- 服务安装需要管理员/root 权限

## 注意事项

- 配置文件 `config.db` 包含敏感信息（API Key、TOTP Secret），请勿提交到版本控制
- 历史记录 `history.json` 可能包含敏感数据，注意保护
- TLS 证书目录 `.switchai/certs/` 包含私钥，妥善保管
- 将 `ca.pem` 导入系统受信任根证书存储后浏览器将不再显示安全警告
- 建议在本地网络环境下使用，不要暴露到公网
- Token 统计数据仅保存在内存中，重启服务后会清空
- 请求历史持久化到文件，重启后会自动加载
- HTTPS 测试模式下使用 `InsecureSkipVerify`，仅推荐开发/内网使用

## License

MIT
