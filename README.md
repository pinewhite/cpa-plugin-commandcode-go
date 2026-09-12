# Command Code Go Plugin for CLIProxyAPI

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go)](https://go.dev/)
[![CLIProxyAPI Compatible](https://img.shields.io/badge/CLIProxyAPI-v7.2%2B-brightgreen)](https://github.com/router-for-me/CLIProxyAPI)

An enterprise-ready **CLIProxyAPI (CPA)** native shared-library plugin (`.so`) designed to integrate **Command Code** models (especially the $1 Go tier and higher tiers) seamlessly into standard OpenAI and Anthropic compatible interfaces.


> **⚠️ 免责声明 / Disclaimer**
> 1. **用途说明**：本项目仅供技术研究、个人学习交流与协议兼容性测试使用，严禁用于任何商业滥用行为。
> 2. **非官方项目**：本项目与 Command Code 官方无任何隶属、赞助、授权或背书关系。相关名称及商标均属于各自持有者所有。
> 3. **合规自负**：使用者需严格遵守上游服务商的服务条款 (Terms of Service)。对于因使用本项目产生的任何账号限流、封禁、配额损失或其他任何纠纷，本项目作者概不负责。
>
> *(This project is for educational and technical research purposes only. It is not affiliated with or endorsed by Command Code. Use at your own risk.)*

---

## 💡 Overview & Background

The Command Code **$1 Go tier** exclusively exposes the CLI-specific `/alpha/generate` streaming endpoint instead of the standard `/provider/v1` API. Standard proxy gateways cannot interface with it directly because:
1. It mandates Server-Sent Events (SSE) streaming with specialized event envelopes.
2. It requires client authentication headers matching the Command Code CLI runtime (`x-command-code-version`, `x-project-slug`, etc.).

This plugin serves as a high-performance protocol bridge and load-balancing engine:
- **Full Bidirectional Protocol Translation**: Translates standard OpenAI `/v1/chat/completions` and Anthropic `/v1/messages` requests to `/alpha/generate` stream payloads, streaming back SSE chunks in real time.
- **Smooth Weighted Round-Robin (SWRR)**: Multi-account credential pool with dynamic weight adjustment and failover.
- **Embedded Visual Management Portal**: Rich bilingual (中/EN) WebUI (`/admin`) for credential binding, one-click OAuth authorization, and real-time credential pool management.
- **Zero-Data-Retention (ZDR) Support**: Native support for privacy-first routing (`x-cmd-zdr: 1`).

---

## 🏛 Architecture

```text
+-------------------------------------------------------------------------+
|                              Client Layer                               |
|        (OpenAI SDK, Anthropic Claude Code, Chatbox, NextChat, etc.)     |
+------------------------------------+------------------------------------+
                                     | Standard API (/v1/chat/completions)
                                     v
+-------------------------------------------------------------------------+
|                            CLIProxyAPI (Host)                           |
+------------------------------------+------------------------------------+
                                     | CGO Plugin ABI (c-shared)
                                     v
+-------------------------------------------------------------------------+
|                  Command Code Go Plugin (This Project)                  |
|                                                                         |
|  +---------------------+  +---------------------+  +-----------------+  |
|  | Request Translator  |  | SWRR Scheduler      |  | Embedded WebUI  |  |
|  | OpenAI -> /alpha/.. |  | Multi-Key Balancing |  | (/admin)        |  |
|  +---------------------+  +---------------------+  +-----------------+  |
+------------------------------------+------------------------------------+
                                     | HTTPS / SSE (EventStream)
                                     v
+-------------------------------------------------------------------------+
|                  Command Code API (api.commandcode.ai)                  |
|                           (/alpha/generate)                             |
+-------------------------------------------------------------------------+
```

---

## ✨ Features

- 🔄 **Protocol Translation**: Automatic conversion between standard chat completion requests and Command Code event stream.
- ⚖️ **Smooth Weighted Round-Robin**: Weighted load balancing across multiple `user_...` accounts with real-time health checks.
- 🖥️ **Embedded Admin Portal**: Built-in responsive HTML5 dashboard with zero external assets, dark/light theme toggle, and English/Chinese i18n.
- 🔑 **Automatic CPA Credential Sync**: Writes `cmdc-*.json` files directly into CPA's `auth-dir` for seamless host integration.
- 🛡️ **Zero Data Retention**: Optional header `x-cmd-zdr: 1` prevents upstream prompt logging.
- 🚀 **Zero Runtime Dependencies**: Compiles to a single `.so` file loaded by CLIProxyAPI.

---

## 🛠️ Build & Installation

### Prerequisites
- **Go**: 1.22 or later (1.24+ recommended)
- **CGO**: Enabled (`CGO_ENABLED=1`)
- **GCC / Clang**: C compiler installed on host

### 1. Build the Shared Library Plugin
```bash
# Clone the repository
git clone https://github.com/your-org/cpa-plugin-commandcode-go.git
cd cpa-plugin-commandcode-go

# Download Go module dependencies
go mod download

# Build as c-shared plugin
CGO_ENABLED=1 go build -buildmode=c-shared \
  -ldflags "-X main.pluginVersion=0.1.0 -s -w" \
  -o commandcode-go.so .
```

*Note: This generates `commandcode-go.so` and `commandcode-go.h`.*

### 2. Configure CLIProxyAPI
Copy `config.example.yaml` to your CLIProxyAPI directory or integrate it into `config.yaml`:

```yaml
plugins:
  commandcode-go:
    base_url: "https://api.commandcode.ai"
    cli_version: "0.1.34"
    project_slug: "cpa-commandcode"
    zdr: false
    api_keys:
      - key: "user_xxxxxxxxxxxxxxxxxxxxxxxx"
        weight: 1
        label: "primary-account"
      - key: "user_yyyyyyyyyyyyyyyyyyyyyyyy"
        weight: 2
        label: "secondary-account"
    models:
      - alias: "commandcode-go/Qwen/Qwen3.7-Flash"
        name: "Qwen/Qwen3.7-Flash"
        display_name: "Qwen 3.7 Flash"
```

### 3. Run with CLIProxyAPI
Start CLIProxyAPI pointing to the compiled `.so` plugin. Access the admin dashboard at:
```text
http://<your-cpa-host>:<port>/admin
```

---

## 🔒 Privacy & Security Best Practices

When deploying this plugin in production:
1. **Never Commit Sensitive Credentials**: Keep all `config.yaml`, `cmdc-*.json`, and `.env` files out of source control. Use the provided `.gitignore`.
2. **Reverse Proxy & Access Control**: The `/admin` and `/api/accounts` routes should never be exposed directly to public internet without reverse-proxy authentication (e.g., HTTP Basic Auth, Nginx `auth_basic`, or VPN/Tailscale).
3. **Log Sanitization**: Ensure reverse proxies do not log query strings or request headers containing API keys.

---

## 📄 License

This project is licensed under the [Apache License 2.0](LICENSE).
