# DeepAICode2API

[![ci](https://github.com/2951461586/DeepAICode2API/actions/workflows/ci.yml/badge.svg)](https://github.com/2951461586/DeepAICode2API/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.24-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![deps](https://img.shields.io/badge/deps-0%20(stdlib)-00ADD8)](go.mod)
[![tests](https://img.shields.io/badge/tests-260%20cases-brightgreen)](https://github.com/2951461586/DeepAICode2API/actions/workflows/ci.yml)
[![protocol](https://img.shields.io/badge/protocol-OpenAI%20%7C%20Anthropic-6b4fbb)](#三接口清单)
[![docker](https://img.shields.io/badge/docker-ready-2496ED?logo=docker&logoColor=white)](docker-compose.yml)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/2951461586/DeepAICode2API)

把本机 **DeepAI Code 桌面端**的上游模型服务，逆向复刻成一个
**OpenAI / Anthropic 双协议兼容的 Go 网关**。

复用桌面端登录后写入的 API Key，无需另申请密钥；纯 Go 标准库实现，
单文件约 10 MB，零外部依赖。

> ⚠️ 上游从 0.3.19（2026-11）起把 DeepAI Code 推理收窄到 **Google/Apple 登录**的账号：
> 邮箱密码注册的号会在 `/theos` / 推理端点拿到 `403 social_login_required`。
> 网关会把这类账号标记为 `blocked` 并跳过调度；邮箱注册链路仍能产号，但已无法推理。

| 能力 | 说明 |
| --- | --- |
| 双协议 | OpenAI `/v1/chat/completions`、`/v1/responses`；Anthropic `/v1/messages` |
| 协议对齐 | 车道 4 + `X-DeepAI-Code-Instance` + `/theos` 签到；区分 `social_login_required` / `machine_required` / `lane_version_unsupported` |
| 号池治理 | 分组轮询、失败换号、容量退避、按账号配额调度、策略拦截自动跳过 |
| 账号注册 | 邮箱注册 → 登录 → 取 Key → 验活 → 入池，CF 临时邮箱 + 代理池；入池前识别社交-only / 策略拦截 |
| 控制台 | 单页内嵌 UI：账号 / 密钥 / 日志 / 统计 / 注册 / 设置 |
| 安全 | 密钥鉴权、管理口令、同源写保护、状态文件 AES-256-GCM 可选加密 |
| 可回归 | 260 个用例 / 7.2k 行测试，`go test -race` + gofmt / vet / build / vulncheck 全绿，CI 覆盖 9 个包 |

```text
┌──────────────┐  /v1/chat/completions   ┌────────────────┐   Api-Key + Theos 头   ┌─────────────────────────────┐
│ OpenAI SDK   │  /v1/responses          │ DeepAICode2API │  X-DeepAI-Code-Machine │ https://api.deepai.org      │
│ Claude Code  │  /v1/messages           │   （本网关）    │ ─────────────────────▶ │ /deepai_code/chat/completions│
│ pi / Cline   │  /v1/models             │                │  ◀───────────────────── │ /deepai_code/responses      │
└──────────────┘  SSE 流式 / 非流式       └───────┬────────┘      OpenAI 兼容 SSE    └─────────────────────────────┘
                                                  │ 自动读取
                                                  ▼
                          ~/.deepai-code/userdata/secrets/deepai-account.bin
                          {"apiKey":"…","username":"…","isPro":false}
```

---

## 一、逆向结论

DeepAI Code 是 **Electron + 本地 t3code server** 架构（应用版本 0.3.20，车道 4）。
真正干活的不是桌面壳，而是 `resources/server.asar` 里的 t3code server；
它把上游能力封装成本地回环 HTTP 服务，再交给 Pi（编码 agent）调用。

### 1.1 关键事实

| 环节 | 逆向结果 |
| --- | --- |
| 应用形态 | Electron 壳（`app.asar`）+ 独立 server（`server.asar`，约 200 MB） |
| 数据目录 | `T3CODE_HOME` 或 `~/.deepai-code`，状态目录为 `<base>/userdata` |
| **凭据落盘** | `<stateDir>/secrets/deepai-account.bin`，**明文 JSON**（文件权限 0600） |
| 凭据内容 | `{apiKey, username, isPro}`，`apiKey` 为 UUID 格式 |
| **推理入口** | `POST https://api.deepai.org/deepai_code/chat/completions` |
| Responses 入口 | `POST https://api.deepai.org/deepai_code/responses`（仅 `api=openai-responses` 模型） |
| 模型目录 | `GET https://api.deepai.org/deepai_code/models` |
| 配额 | `GET https://api.deepai.org/deepai_code/theos` |
| 用量 | `GET https://api.deepai.org/get_usage_summary` |
| 语音转写 | `POST https://api.deepai.org/speech_to_text`（字段 `voiceRecording`） |
| 联网搜索 | `POST https://api.deepai.org/deepai_code/web_search` |
| **鉴权** | 请求头 `Api-Key: <apiKey>`（不是 `Authorization`） |
| 协议头 | `X-DeepAI-Code-Theos: 4`（计费车道版本；2/3/5 一律 426） |
| 进程实例 | `X-DeepAI-Code-Instance: <32 位 hex>`（每个出口代理随机一次，随该代理的所有 JSON 请求发出；桌面端是每进程一次） |
| 签到 | `/theos` 响应带 `check_in:{nonce, seconds}`；下次 `/theos` 回传 `X-DeepAI-Code-Check-In` 与 `X-DeepAI-Code-Beacon: v=…;up=…;pi=0\|1`（`focus` 只在焦点状态已知时才追加） |
| **机器指纹** | `X-DeepAI-Code-Machine: sha256(原始机器 UUID)`，用于按天发放免费 Theos；计量模型缺失时回 `machine_required` |
| 会话粘性 | `X-Session-Affinity`（body 里的 `prompt_cache_key` 也会被上游使用） |
| 容量拒绝 | `409 {"error":{"code":"session_busy"}}` + `Retry-After` |
| 账号策略 | `403 social_login_required`：上游只允许 Google/Apple 登录的账号推理（邮箱密码号被挡） |
| 浏览器登录 | `GET /deepai_code/desktop_auth/?state=…&port=…` → 本机 `/auth-callback?code=…` → `POST /deepai_code/desktop_auth/exchange/` |

### 1.2 上游本身就是 OpenAI 兼容的

`chat/completions` 的请求 / 响应与 OpenAI 完全一致，响应还额外带了
OpenRouter 风格的 `provider`、`reasoning`、`reasoning_details`、`cost` 等字段
（`id: gen-…`、`usage.cost_details.upstream_inference_*`、`native_finish_reason`）。
注意 `provider` 是**真实推理后端名**（`InferenceNet` / `Wafer` / `Reka` / …），
只有它等于 `"DeepAI"` 才代表这次请求被换到了替身后端（见 §10.1）。
因此本网关不需要做复杂的协议还原，重点在：

1. **凭据导入**：从桌面端明文凭据文件读取并去重；
2. **账号池**：多 Key 轮询、失败换号、`session_busy` 等待重试；
3. **模型治理**：动态目录缓存、别名、兜底模型；
4. **协议适配**：为 Anthropic 客户端和 Responses 客户端做翻译；
5. **可观测性**：请求日志、token / 成本统计、配额探测、控制台。

### 1.3 机器指纹怎么来的

桌面端逻辑（`src/deepai/machineId.ts`）：

- Windows：`(Get-CimInstance Win32_ComputerSystemProduct).UUID`，固件占位 UUID 时回落
  到注册表 `HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid`；
- macOS：`ioreg` 的 `IOPlatformUUID`；
- Linux：`/sys/class/dmi/id/product_uuid`，其次 `/etc/machine-id`；
- 最终取 `sha256(小写原始值)`，原始值绝不上传。

本网关复刻了同一套逻辑。容器 / 无 SMBIOS 环境可用
`DEEPAI_MACHINE_ID=<原始UUID>` 显式覆盖。

---

## 二、支持的模型

模型目录**完全由上游下发**（当前 18 个，下表是 2026-11-08 实测的快照），
网关只做缓存与别名，上游新增模型无需改代码。`Theos/次` 取自上游
`theosPerRequest`，`0` 即免费（9 个免费 / 9 个付费）。

| ID | 名称 | 上下文 | 最大输出 | Theos/次 | 说明 |
| --- | --- | ---: | ---: | ---: | --- |
| `glm-5.3-flash` | GLM 5.3 Flash | 1,048,576 | 131,072 | **0** | 默认模型，免费 |
| `deepseek-v4.1-flash` | DeepSeek V4.1 Flash | 1,048,576 | 131,072 | **0** | 免费 |
| `mimo-2.6-flash` | MiMo 2.6 Flash | 1,048,576 | 131,072 | **0** | 免费 |
| `solar-mini-4` | Solar Mini 4 | 524,288 | 131,072 | **0** | 免费，纯文本 |
| `gpt-6-luna` | GPT-6 Luna | 1,048,576 | 128,000 | **0** | 免费 |
| `mimo-2.6-pro` | MiMo 2.6 Pro | 1,048,576 | 131,072 | **0** | 免费 |
| `crouton-alpha` | Crouton Alpha | 1,048,576 | 131,072 | **0** | 免费 |
| `glyph-cluster` | Glyph Cluster | 256,000 | 131,072 | **0** | 免费，纯文本 |
| `deepai-cyber` | DeepAI Cyber | 65,536 | 8,192 | **0** | 免费，纯文本 |
| `glm-5.3` | GLM 5.3 | 1,048,576 | 131,072 | 3 | 付费，纯文本 |
| `kimi-k3` | Kimi K3 | 1,048,576 | 131,072 | 3 | 付费 |
| `muse-spark-1.3` | Muse Spark 1.3 | 1,048,576 | 131,072 | 6 | 付费 |
| `gpt-6.1-sol` | GPT-6.1 Sol | 1,048,576 | 128,000 | 10 | 付费 |
| `gemini-4-argon` | Gemini 4 Argon | 1,048,576 | 131,072 | 10 | 付费 |
| `claude-opus-5-5` | Claude Opus 5.5 | 1,000,000 | 64,000 | 15 | 付费 |
| `muse-wonder` | Muse Wonder | 1,048,576 | 131,072 | 15 | 付费 |
| `claude-fable-5-1` | Claude Fable 5.1 | 1,000,000 | 64,000 | 25 | 付费 |
| `gpt-6-astra` | GPT-6 Astra | 1,048,576 | 128,000 | 50 | 付费，仅 `/v1/responses` |

说明：

- 除 `solar-mini-4` / `glyph-cluster` / `glm-5.3` / `deepai-cyber` 外，
  其余模型都接受图片输入（`input: [text, image]`），且全部支持推理（`reasoning`）；
- `gpt-6-astra` 的 `api` 是 `openai-responses`，必须走 `/v1/responses`，
  用 `/v1/chat/completions` 调用会被上游拒绝；
- 付费模型按 `Theos/次` 计量，从账号的每日额度 / 余额里扣；额度耗尽后上游返回 403（见 §6.9）。

内置别名（可在控制台覆盖）：

- `gpt-4o` / `gpt-4` / `gpt-3.5-turbo` / `claude-3-5-sonnet` / `deepseek-chat` … → `glm-5.3-flash`
- `deepai/<id>`、`openai/<id>` 前缀会被自动剥离

无法识别的模型名**默认**回落到「兜底模型」→ 目录默认模型，保证任意客户端开箱即用；
控制台打开「**严格模型模式**」后改为返回 `404 invalid_request_error`，并列出相近的
目录 ID，不再静默替换（模型指纹 / 渠道核对场景一定要开）。

---

## 三、接口清单

### OpenAI 兼容

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/v1/models` | 模型目录（含 `deepai` 元信息） |
| POST | `/v1/chat/completions` | 流式 / 非流式；支持 tools、图片、`prompt_cache_key` |
| POST | `/v1/responses` | `openai-responses` 模型透传；其余模型自动翻译 |
| POST | `/v1/audio/transcriptions` | 转发上游语音转写（webm / mp4） |
| POST | `/v1/audio/translations` | 同上（别名） |

### Anthropic 兼容

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/v1/messages` | 流式 / 非流式，支持 `tool_use`、`thinking`、图片 |
| POST | `/v1/messages/count_tokens` | 本地估算（上游不提供） |

Claude Code 只需：

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:10087
export ANTHROPIC_API_KEY=sk-...        # 网关密钥；未签发时可留空
export ANTHROPIC_MODEL=glm-5.3-flash
```

### DeepAI 原生能力扩展

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/v1/deepai/theos` | 当前账号免费额度 / 余额 |
| GET | `/v1/deepai/usage` | 订阅与用量摘要 |
| POST | `/v1/deepai/web_search` | 上游联网搜索（`{query, num_results}`） |
| POST | `/v1/deepai/find_service` | 上游服务推荐 |

### 管理接口（注册 / 入池）

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/admin/api/register` | 启动批量注册任务（`count` / `concurrency` / `group` / `mail_backend` / `email` / `password` / `verify`） |
| GET | `/admin/api/register?id=…` | 查询任务进度与逐账号结果 |
| POST | `/admin/api/accounts` | `action=enroll`：验活并写入号池 |
| POST | `/admin/api/checkin` | 配额探测。可选 `id` / `group` / `stale_only` / `limit`；响应带 `total` / `probed` / `remaining`，按批推进 |

### 控制台

| 路径 | 说明 |
| --- | --- |
| `/` | 内嵌单页控制台（无外部依赖） |
| `/ping` | `pong` |
| `/health` | 账号 / 模型 / 机器指纹摘要 |
| `/admin/api/*` | 账号、密钥、日志、统计、设置、配额、登录 |

---

## 四、快速开始

```bash
# 1) 编译（产物在 bin/；也可直接 go build -o deepaicode2api.exe .）
make build

# 2) 直接启动（会自动导入桌面端凭据 + 拉取模型目录）
./bin/deepaicode2api

# 3) 打开控制台
#    http://127.0.0.1:10087/

# 4) 注册链路（可选）
cp .env.example .env             # 填 TEMPMAIL_* 或 DEEPAI_REGISTER_DOMAIN
go build -o register.exe ./cmd/register
./register.exe -doctor          # 检查邮箱后端 / 网关 / 机器指纹
./register.exe -count 1         # 注册 1 个账号 → 验证 → 验活 → 入池
./register.exe -count 3 -concurrency 2 -group batch-a
./bin/register -check-proxies   # 检查代理池 → 上游连通性
./bin/register -reverify -group <分组>  # 给未验证账号补点验证链接（限流）
./bin/register -resume          # 补跑台账里未入池的账号
```

> **`.env` 现在由网关在启动时加载**（此前只有注册 CLI 惰性加载，
> 写进 `.env` 的 `DEEPAI2API_MASTER_KEY` 对主进程不生效）。
> 状态文件里是上游 API Key 与账号密码，Windows 上 `0600` 不设 ACL，
> 因此首次部署就应开启加密。**先停掉正在运行的网关**，再执行：

```bash
printf 'DEEPAI2API_MASTER_KEY=%s\n' "$(openssl rand -hex 32)" >> .env
./bin/deepaicode2api -seal -yes   # 把存量明文（含 append-only 历史日志）封存
./bin/deepaicode2api              # 之后每次保存都自动加密
```

`-seal` 会打印密钥指纹（sha256 前 8 位）便于核对，并校验每个文件不再含明文；
**主密钥必须与备份一起保管，丢失后号池不可恢复**。
只改环境变量而不跑 `-seal` 时，主文件会在下一次保存时自动重写为密文，
但历史日志行会保持明文（读取端兼容混合格式，不会报错）。

启动日志示例：

```text
状态文件: …/deepaicode2api-data.json
机器指纹: a1b2c3d4e5f6…（用于上游按天发放免费 Theos）
已导入账号 desktop-…（~/.deepai-code/userdata/secrets/deepai-account.bin）
模型目录已更新：18 个模型，默认 glm-5.3-flash
DeepAICode2API dev 已启动: http://127.0.0.1:10087/
```

### 调用示例

```bash
# OpenAI 风格
curl http://127.0.0.1:10087/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}'

# 流式
curl -N http://127.0.0.1:10087/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

Python OpenAI SDK：

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:10087/v1", api_key="sk-...")
print(client.chat.completions.create(
    model="glm-5.3-flash",
    messages=[{"role": "user", "content": "hello"}],
).choices[0].message.content)
```

---

## 五、命令行参数与环境变量

| 参数 | 环境变量 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `-host` | `DEEPAI2API_HOST` | `127.0.0.1` | 监听地址 |
| `-port` | `DEEPAI2API_PORT` | `10087` | 监听端口 |
| `-data` | `DEEPAI2API_DATA_PATH` | 用户配置目录 | 状态文件 |
| `-deepai-dir` | `DEEPAI2API_DATA_DIR` | 自动探测 | 桌面端数据目录 |
| `-admin-password` | `DEEPAI2API_ADMIN_PASSWORD` | 空 | 管理密码 |
| `-allow-insecure` | `DEEPAI2API_ALLOW_INSECURE` | `false` | 允许非回环监听但不设管理密码 |
| `-max-inflight` | `DEEPAI2API_MAX_INFLIGHT` | `2` | 单号同时在途的上游请求上限（总槽位 = 账号数 × 该值） |
| `-admission-wait` | `DEEPAI2API_ADMISSION_WAIT` | `2s` | 槽位全满时的排队上限，超时才回 429（`0` 立即拒绝） |
| `-import` | — | — | 仅导入凭据后退出 |
| `-no-import` | — | — | 启动时不自动导入 |
| `-seal` | — | — | 把状态/统计/日志中的存量明文封存为密文后退出（需 `DEEPAI2API_MASTER_KEY`） |
| `-yes` | — | — | 配合 `-seal` 确认不可逆操作 |
| `-version` | — | — | 打印版本 |

其他环境变量：

| 变量 | 说明 |
| --- | --- |
| `DEEPAI_API_KEY` | 直接添加一个上游账号（可选） |
| `DEEPAI_MACHINE_ID` | 覆盖机器指纹原始 UUID（容器部署必填） |
| `DEEPAI2API_MASTER_KEY` | 状态文件加密主密钥（64 位 hex / base64，32 字节），留空则明文；写在 `.env` 即可，启动时自动加载 |
| `DEEPAI_API_ORIGIN` | 覆盖上游地址（自建测试） |
| `TEMPMAIL_BASE` / `TEMPMAIL_ADMIN_KEY` / `TEMPMAIL_DOMAIN` | CF Temp Email Worker（注册链路收信） |
| `DEEPAI_REGISTER_DOMAIN` | 静态邮箱域名（无收件箱） |
| `DEEPAI_REGISTER_MAIL_BACKEND` | 强制邮箱后端：`cf` / `static` / `none` |
| `DEEPAI2API_GATEWAY` | `register.exe` 默认入池地址 |
| `DEEPAI2API_ADMIN_PASSWORD` | 管理密码（CLI 入池 / 控制台） |
| `DEEPAI_REGISTER_GROUP` | 默认号池分组 |
| `PROXY_POOL` | 注册 / 推理代理池（逗号或换行分隔完整 URL） |
| `PROXY_POOL_FILE` | 代理池文件（每行一个） |
| `DEEPAI_REGISTER_PROXY` | 单代理（覆盖代理池） |
| `DEEPAI2API_ENV_FILE` | 指定 `.env` 路径 |
| `T3CODE_HOME` / `DEEPAI_CODE_HOME` | 桌面端数据目录 |

### 诊断工具

```bash
go run ./cmd/probe -usage                 # 凭据 + 模型目录 + 配额
go run ./cmd/probe -prompt "hello"        # 真实调用一次
go run ./cmd/probe -prompt "hi" -stream   # 流式调用
go run ./cmd/probe -key <api-key>         # 直接验证某个 Key
```

---

## 六、注册 → 登录 → 取 Session → 验活 → 入池

本项目把 `deepai.org.har` 里的网页注册链路复刻成了纯 HTTP 的自动化流水线，
产物直接写入 DeepAICode2API 的轮询号池。

### 6.1 HAR 逆向结论

| 阶段 | 请求 | 说明 |
| --- | --- | --- |
| 预检 | `POST /get_user_login_type`（multipart） | 字段 `email`；返回 `user_exists` / `login_with_password` |
| 注册来源 | `POST /save_custom_signup_data`（JSON） | 对齐网页端，失败可忽略 |
| 注册 | `POST /daily-time-sync/registration/`（multipart） | `email` / `username` / `password1` / `password2`；201 后下发 `sessionid` |
| 登录 | `POST /daily-time-sync/login/`（multipart） | `username`（邮箱）/ `password`；400 = 凭据错误 |
| 取 Session | `GET /daily-time-sync/user/` | 返回 `pk`、`username`、`userprofile.api_key`、`plan` |
| 邮箱验证 | `GET /verify_email?key=…` | 链接来自验证邮件；验证后每日 Theos 才会发放 |
| 验活 | `GET /deepai_code/theos` | 带 `Api-Key` + `X-DeepAI-Code-Machine`，200 即可用 |

关键点：

- `userprofile.api_key` **就是** DeepAICode2API 使用的上游 Key；
- 注册 / 登录都是普通 Cookie 会话，不需要浏览器、不需要 CSRF 头；
- 邮箱验证后 `/theos` 才会返回 `granted > 0`；
- 每日 Theos 按“机器指纹 + 账号”发放：同一台机器当天已领过额度时，
  新账号的 `granted` 可能为 `0`，但免费模型仍然可用。

### 6.2 链路与状态

```text
建临时邮箱 → 预检邮箱 → 注册 → 登录 → 取 Session/API Key
           → 收信验证（可选）→ /theos 验活 → 写入 2API 号池 → 台账/交付物
```

| 状态 | 含义 |
| --- | --- |
| `registered` | 会话已建立，已取到 API Key |
| `verified` | 邮箱验证链接已访问 |
| `keyed` | `/theos` 验活通过（可入池但尚未入池） |
| `enrolled` | 已写入号池 |
| `failed` | 关键阶段失败 |

台账按邮箱并集合并：高等级记录胜出，已赚到的 `api_key` / `account_id` 不会被后续失败覆盖。

### 6.3 CLI

```bash
./register.exe -doctor                     # 环境体检（不出网注册）
./register.exe -count 1                    # 注册 → 验证 → 验活 → 入池
./register.exe -count 5 -concurrency 2 -group batch-a
./register.exe -email a@b.com -password '...'   # 复用已有邮箱（count=1）
./register.exe -resume                     # 补跑台账里未入池的账号
./register.exe -no-enroll                  # 只注册/验活，不写号池
./register.exe -mail-backend static -domain example.com   # 无收件箱，跳过验证
```

常用参数：`-gateway`、`-admin-password`、`-ledger`、`-result-dir`、
`-verify=false`、`-probe-chat`、`-timeout`、`-delay`。

### 6.4 控制台

控制台新增「注册」页：

- 数量 / 并发 / 分组 / 邮箱域名 / 邮箱后端 / 自动验证；
- 启动后台任务，实时轮询每个账号的阶段、Key、Theos、错误；
- 完成后自动刷新账号池。

### 6.5 邮箱后端

| 后端 | 能力 | 配置 |
| --- | --- | --- |
| `cf` | 建邮箱 + 收信 + 自动点验证链接 | `TEMPMAIL_BASE` / `TEMPMAIL_ADMIN_KEY` / `TEMPMAIL_DOMAIN` |
| `static` | 随机前缀 `@域名`，无收件箱（跳过验证） | `DEEPAI_REGISTER_DOMAIN` |
| `none` | 使用 `-email` 指定的固定邮箱 | 显式 `-email` |

留空自动选择：配置了 CF 就用 `cf`，否则有域名就用 `static`，否则 `none`。

### 6.6 台账与交付物

- `exports/register-ledger.jsonl`：全部尝试（含失败），按邮箱合并；
- `exports/register-ledger-credentials.jsonl`：**仅**密码 / Session / CSRF，供 `-resume` 使用；
- `result/keys.txt`：`email----api_key----account_id`；
- `result/apikeys.txt`：纯 API Key，一行一个；
- `result/success.jsonl`：机器可读成功记录（已脱敏，不含密码）。

台账与交付物写入前会过一遍 `redactRecord`：`password` / `session_key` /
`csrf_token` 不会出现在主台账与 `success.jsonl` 里，因此台账可以直接贴进
issue 或发给别人排查。凭据单独落在 `*-credentials.jsonl`（`0600`，读取时按
邮箱并回），不需要补跑时可直接删掉这一个文件。

这些都已在 `.gitignore` / `.dockerignore` 中排除。

### 6.7 号池分组与轮询

- 账号有 `group`（默认 `default`）；
- API Key 也有 `group`，请求进来后只会在对应分组内轮询；
- 未绑定分组的 Key 使用全部分组；
- 管理员在控制台创建密钥时可指定分组。

这样可以用一个网关同时服务多个批次 / 租户，而不会串号。

### 6.8 代理池与指纹库

- 注册与推理共用同一代理池：`PROXY_POOL`（逗号 / 换行分隔的完整 URL）或
  `PROXY_POOL_FILE`（每行一个）；`DEEPAI_REGISTER_PROXY` 可指定单代理；
- 每个账号轮询分配一个代理并写入台账，补跑时复用历史代理；
- 推理侧按账号绑定的代理调用上游（`Server.clientFor`），管理接口只展示
  `host:port`，不泄露代理凭据；
- 存量账号（HAR 导入 / 手工导入 / 桌面端导入）可随时从代理池补绑：
  `POST /admin/api/accounts {"action":"assign-proxy"}`
  （默认只补空值；`"reassign":true` 覆盖、`"clear":true` 回滚；
  可用 `"id"` 或 `"group"` 限定范围）；
- 多样化出口有价值：每日免费 Theos 是**按网络（出口 IP）限额**发放的
  （见 `docs/recon.md` 的 `network_bound`），把 14 个号分散到 12 个出口后，
  有额度的账号从 5 个提升到 10 个；
- 指纹库内置 6 套自洽的 `User-Agent` / `Sec-CH-UA` / 平台 / `Accept-Language`，
  按邮箱稳定分配（同一账号补跑得到同一套请求头）；
- CLI：`-check-proxies`、`-proxy-pool`、`-proxy-file`、`-proxy`、`-no-fingerprint`、
  `-reverify`（补点验证链接，见 `docs/lessons.md` §2.3）；
- 控制台「注册」页可直接粘贴代理池并关闭指纹库。

### 6.9 机器指纹（为什么每号一个）

上游按 `X-DeepAI-Code-Machine` **每天发放一次**免费 Theos，因此：

- 每个账号在注册时生成一个独立的机器 UUID（`deepai.NewMachineUUID`），
  存入台账与号池，之后 `/theos` 与推理全程使用它（`Account.MachineID`）；
- 没有账号级指纹时才回退到进程级 `MachineID()`（桌面端导入 / 历史账号）；
- 共用指纹时，同一台机器上的多个账号会互相抵消额度 —— 这是旧版本
  池内 `granted=0` 的根因；
- 存量账号可一键补齐（只补空值，不覆盖已有指纹）：

```bash
curl -s -X POST http://127.0.0.1:10087/admin/api/accounts \
  -H "X-Admin-Password: $DEEPAI2API_ADMIN_PASSWORD" \
  -d '{"action":"assign-machine"}'
# {"ok":true,"assigned":20}
```

补齐后会自动异步重探配额。调度侧也会优先挑有额度的账号（`hasQuota`），
已知 `balance=0` 的账号只在全组都没额度时才兜底。

### 6.9.1 额度自动复探

上游按 `refresh_at` 每天发放一次额度。早期只能人工点「刷新全部配额」，
653 个号一次要 11 分钟，还会因为关掉页面（`r.Context()` 取消）中断一半。
现在三层自动机制覆盖：

| 层 | 触发 | 范围 | 失败处理 |
| --- | --- | --- | --- |
| 入池复探 | 入池后 45s / 2min / 6min | 新账号 | 采信成功结果 |
| 按需复探 | 推理选中「快照已过期」的账号 | 仅用到的号，每 15min 最多一次 | 不改 `Status` / `Failures`（上游抖一下不该被调度误判） |
| 后台守护 | 每 5 分钟一批（≤24 个） | `refresh_at` 已过 / 从未探测 / 超 6h 未探 | 同上 |

三条路径共享一张在途去重表与**全局 4 个探测槽位**：653 个号的快照同时过期时，
不会因为一波流量就叠出上百个并行 `/theos`（那是最容易被风控的形态）。
拿不到槽位的会在下一轮补上。

`/admin/api/checkin` 改为分批 + 进度语义：

```bash
# 只探 g2 分组里额度已过时的账号，本批最多 50 个
curl -s -X POST localhost:10087/admin/api/checkin \
  -H "X-Admin-Password: $DEEPAI2API_ADMIN_PASSWORD" \
  -d '{"group":"g2","stale_only":true,"limit":50}'
# → {"ok":true,"total":120,"probed":50,"remaining":70,"results":[…]}
```

控制台「刷新全部配额」会按 `remaining` 自动翻页，直到整池都探过；
`/health` 额外给出 `quota_fresh`（15 分钟内探过）与 `quota_due`（守护会去重探）。

### 6.10 实测：补齐机器指纹前后的额度对比

同一份号池（21 个账号：1 个桌面端导入 + 20 个批量注册，11 个代理），
用 `assign-machine` 补齐前后各跑一次 `/admin/api/checkin`：

| 指标 | 补齐前 | 补齐后 |
| --- | --- | --- |
| 有独立机器指纹的账号 | 0 / 21 | **21 / 21**（21 个互不相同） |
| `balance > 0` 的账号 | **0 / 21** | **20 / 21** |
| 典型配额 | `balance=0.00000 granted=0.00000` | `balance=200.00000 granted=200.00000` |
| 配额刷新 | — | `refresh_at=2026-11-07T04:03:03Z`（次日 UTC 04:03） |
| 连续 8 次真实推理 | — | **8/8 HTTP 200**，落在 **8 个不同账号** |

唯一 `balance=0` 的是桌面端导入的账号（`source=desktop`，单独在
`default` 分组）——它当天在原机器指纹下的额度已被先前使用消耗。

这说明此前的 `granted=0` 不是上游策略收紧，就是**共用机器指纹**导致的：
同一指纹每天只发一次额度。补齐后每个账号各自领取 200 单位。

> 注意：`/theos` 的 `balance` 精度是 5 位小数，单次请求成本约 `1.8e-6`，
> 所以小额调用看不出余额下降；要看消耗请查 `/v1/deepai/usage`。

### 6.11 工程经验与验收基线

第 1~10 批 50 号压测（2026-11）的逐批执行日志已从本文移出，只保留
**可复用结论**；完整规则见 `docs/lessons.md`，测量数据见 `docs/performance.md`。

| 经验 | 结论 | 代码落点 |
| --- | --- | --- |
| 关键路径 | 只有「决定号能不能用」的阶段留在关键路径；邮件等待、验证点击、额度复探全部后台化 | `pipeline.go` / `pipeline_verify.go` |
| 上游预算 | 重 POST 有 45s 总预算，退避可被 ctx 打断；504 用登录兜底 | `web.go` `registerPostBudget` |
| 重端点限流 | `/verify_email` 单次占上游约 13s，全局并发 2 + 2s/10s/60s 退避 | `reverify.go` `verifyClickSlots` |
| 额度语义 | `balance=0` 视为「未知」，15 分钟后重新可调度；三层复探 + 全局 4 槽位 | `pool.go` `QuotaStaleAfter` / `probe.go` |
| 机器指纹 | 每号一份，共用时只有首个账号能领到每日额度 | `Account.MachineID` |
| 观测口径 | 看单号 E2E 中位（架构稳定性）与 p90（依赖健康度），不看批次均值 | `Record.Timings` |

当前验收基线（并发 10 / 12 出口 / 50 号）：

| 指标 | 基线 |
| --- | --- |
| 入池 | **50/50**，零失败（不需要 `-resume`） |
| 单号 E2E 中位 | **8.9~12.3s**（不随上游抖动明显变化） |
| 注册段吞吐 | **45~47 号/分**（≈1.3s/号） |
| 额度 | 批次内当场 47/50，其余由延迟复探在 10 分钟内补齐 |
| 邮箱验证 | 批次内 ≥49/50，余量用 `register -reverify -group <分组>` 补 |

> 基线只用于回归判断：低于基线的批次先查出口与上游端点，再怀疑网关；
> 同配置复跑两批以上才有对照意义（`docs/lessons.md` §4.4）。

### 6.12 安全与合规

- 注册能力用于管理**自己的** DeepAI 账号；请遵守上游服务条款，控制批次大小与频率；
- 状态文件与台账权限 `0600`；但 **Windows 上 `0600` 不设 ACL**，同机其他用户仍可读，
  因此强烈建议配置 `DEEPAI2API_MASTER_KEY` 开启整文件加密（见 §七）；
- 台账与 `success.jsonl` 不含密码；密码只在 `*-credentials.jsonl` 里，可单独删除；
- 不需要补跑时可删除台账或使用 `-no-enroll` / 不保存密码；
- 上游按机器发放每日免费额度，本项目不绕过该限制。

---

## 七、控制台

- **概览**：请求量、失败数、token 入/出、上游成本、模型目录（免费 / 付费、Theos、上下文、端点）。
  Token 与字符按 **K / M / B** 压缩显示（3 位有效数字，悬停看精确值）；模型目录保留精确整数。
- **账号**：导入桌面端凭据、浏览器登录、手工添加 API Key、刷新配额；
  列表支持**分组 / 状态 / 关键字**筛选（服务端分页 + 排序）；
  行内菜单除启停 / 删除 / 清错外，还接了存量治理：**补绑代理 / 清除代理、
  分配机器指纹（只补空值）、改分组、标记已验证**。
- **密钥**：签发 `sk-…` 网关密钥；只在创建时显示一次；可**改绑分组**。
  **未签发任何密钥时，仅本机回环允许无密钥调用；非回环来源一律 `503 setup_required`。**
  若监听非回环地址且尚无密钥，启动时会自动生成一个 `bootstrap` 密钥并打印一次。
- **日志**：端点、模型、流式、状态、耗时、字符数、token、成本、账号；
  支持按**端点 / 账号 / 最小状态码 / 关键字 / 起始时间**过滤（字符与 token 同样是 K/M）。
- **设置**：超时、重试、日志保留、CORS、兜底模型、严格模型模式、模型别名、管理密码。

安全默认值：

- 监听**非回环**地址时必须设置管理密码，否则拒绝启动（如需绕过用 `-allow-insecure`）；
- 未签发密钥时，非回环来源的数据面请求返回 `503 setup_required`，不是放行；
- 未设置管理密码时，管理接口**只允许本机环回**；
- 写操作做同源校验，阻断 CSRF；
- CORS 默认仅允许本机来源，不提供 `*`；
- 状态文件权限 `0600`，控制台只展示脱敏后的 Key。
- 配置 `DEEPAI2API_MASTER_KEY` 后状态文件整体 AES-256-GCM 加密，
  磁盘上不再出现明文上游 Key / 账号密码；已加密的文件在缺少或错配主密钥时
  **拒绝启动**，不会静默重置成空状态。
- `-seal` 会把 append-only 日志里的**历史明文行**一并重新封存（逐行校验、
  幂等）；只改环境变量不会回填旧日志。
- 状态文件默认落在 `os.UserConfigDir()`（Windows `%AppData%`、Unix `~/.config`），
  避免落在 Program Files 或共享盘；旧位置已有文件时会继续沿用，不会丢号池。

---

## 八、架构与目录

```text
main.go                     启动、导入、后台落盘 / 目录刷新 / 额度守护 / 启动摘要
cmd/probe/                  诊断工具
internal/deepai/            上游客户端（协议、SSE、机器指纹、凭据导入）
internal/envfile/           .env 加载（网关与 CLI 共用，启动时生效）
internal/registry/          模型目录解析与别名
internal/store/             状态持久化 + 账号池调度（拆分后单文件 ≤400 行）
  ├─ store_open.go         Open / Save / 落盘快照 / 原子写
  ├─ store_accounts.go     账号 CRUD / 分页 / 调度入口
  ├─ store_keys.go         网关密钥
  ├─ store_models.go       模型目录 / 机器指纹 / 设置
  ├─ store_telemetry.go    统计与日志查询
  ├─ reseal.go             -seal：存量明文（含历史日志）封存
  └─ state_seal.go         AES-256-GCM 信封 / 密钥指纹
internal/register/          注册链路（stages / runner / ledger / mail backend）
  ├─ web.go                DeepAI 网页注册 / 登录 / Session / 验证
  ├─ mailbox.go            CF Temp Email / static / none
  ├─ pipeline.go           单账号阶段 + 入池
  ├─ pipeline_verify.go    验证回填 / 延迟复探 / 验活
  ├─ pipeline_batch.go     批量并发 / 补跑 / 台账归并
  ├─ ledger.go             JSONL 台账（并集合并 / 升级保护）
  ├─ gateway.go            入池 HTTP 客户端
  └─ deliver.go            keys.txt / apikeys.txt / success.jsonl
cmd/register/               注册链路 CLI（doctor / count / resume）
internal/server/            HTTP 路由、协议转换、控制台
  ├─ chat.go                OpenAI Chat Completions（透传 + 归一化）
  ├─ anthropic.go           Anthropic Messages 入口
  ├─ anthropic_translate.go 双向字段映射
  ├─ anthropic_stream.go    流式内容块管理器
  ├─ responses.go           Responses API 入口（透传 / Chat 合成）
  ├─ responses_translate.go Responses ⇄ Chat 字段映射
  ├─ responses_stream.go    Responses 流事件生成器
  ├─ probe.go               额度探测（按需 / 守护 / checkin 分批）
  ├─ extensions.go          配额、用量、搜索、语音
  ├─ login.go               浏览器登录（RFC 8252 本机回调）
  └─ web/index.html         单页控制台
```

分层原则：

- **`deepai`** 只关心上游协议（路径、请求头、错误语义、SSE），不知道号池存在；
- **`store`** 只负责状态与调度（持久化、分组、失败换号、配额判定），不发网络请求；
- **`server`** 只做协议转换与鉴权，账号选择一律通过 `Store`；
- **`register`** 是独立的客户端：它通过管理接口入池，不直接改状态文件。

这样上游协议变动只影响 `deepai`，调度策略变动只影响 `store`，
测试可以分别对着假上游和临时状态文件跑。

---

## 九、部署（Docker）

```bash
docker build -t deepaicode2api .
docker run -d --name deepaicode2api \
  -p 10087:10087 \
  -e DEEPAI_API_KEY=<api-key> \
  -e DEEPAI_MACHINE_ID=<原始机器UUID> \
  -e DEEPAI2API_ADMIN_PASSWORD=<管理密码> \
  -v $PWD/data:/data \
  -e DEEPAI2API_DATA_PATH=/data/deepaicode2api-data.json \
  deepaicode2api
```

> `X-DeepAI-Code-Theos: 4` 是硬门槛：2/3/5 一律 `426 lane_version_unsupported`。
> `X-DeepAI-Code-Machine` 仍是计量模型的必需头：缺失时返回 `400 machine_required`。
> 容器里通常读不到宿主机的 SMBIOS UUID，因此 `DEEPAI_MACHINE_ID` 在容器部署时**必填**。
> 填桌面端所在机器的原始 UUID 即可复用同一份每日免费额度。

---

## 十、限制与说明

- `X-DeepAI-Code-Theos` 必须是 `4`（0.3.19 / 0.3.20 客户端对齐值，两版 wire protocol 一致）；车道不符时上游对所有端点返回 426，需要升级网关；
- 心跳 `X-DeepAI-Code-Beacon` 的 `v=` 必须是当前客户端版本（0.3.20）；
- 推理请求按桌面端实测值发 `user-agent: node` + `accept: */*`，**不带** Chrome UA / `sec-ch-ua` / `Accept-Language`（那些只用于注册链路）；
- `X-DeepAI-Code-Machine` 缺失时计量模型返回 `machine_required`，因此无 SMBIOS 的环境必须设置 `DEEPAI_MACHINE_ID`；
- 上游 0.3.19 起把 DeepAI Code 推理收窄到 **Google/Apple 登录**的账号：
  邮箱密码注册的号会在 `/theos` 或推理端点拿到 `403 social_login_required`。
  网关会把这些账号标记为 `blocked` 并停止调度；控制台里看到该状态时，
  说明该账号只能改用社交登录重新授权，重试/换代理都无效；
- 上游免费额度按「机器指纹 + 账号」每天发放，额度耗尽后返回 403；
  机器指纹**每个账号一份**（见 §6.9），共用时只有第一个账号能领到；
- `gpt-6-astra` 等 `openai-responses` 模型必须走 `/v1/responses`；用
  `/v1/chat/completions` 调用会被上游拒绝（网关会原样回传错误）；
- 语音转写上游只接受 `audio/webm` / `audio/mp4`，其他格式请先转码；
- `/v1/messages/count_tokens` 是字符数估算，不是上游真实 tokenizer；
- 状态文件默认明文保存上游 API Key（与桌面端一致）；配置
  `DEEPAI2API_MASTER_KEY` 后整文件加密，请务必保证主密钥不随备份泄露；
- 注册链路会把上游 API Key、网页密码、Session 与完整代理 URL 写入本机状态文件；
  主台账与 `success.jsonl` 已脱敏，密码只在 `*-credentials.jsonl` 里；
- 代理 URL 含账号密码，必须只放在 `.env` / 本地台账（已 gitignore），不要提交。

### 10.1 已知坑（改动前先看）

以下都是踩过的坑，`docs/recon.md` 有更完整的逆向记录：

| 现象 | 原因 | 不要“修”成什么样 |
| --- | --- | --- |
| 无效 Key 返回 200 且是 HTML | 上游 302 到登录页 | 不要 `CheckRedirect` 跟随；`ErrUseLastResponse` 是刻意的 |
| 换用别的 UA 后「模型变了」 | 上游按 `User-Agent` 分流：非 `node` 的 UA 会被换到另一个后端，但响应 `model` 仍写原名 | 不要“优化”成浏览器 UA；推理必须发 `user-agent: node`（见 `docs/recon.md` §3.1） |
| 响应里出现 `provider: DeepAI` | 说明这次请求落在了被替换的后端（替身实际是 `crouton-alpha` 的 stealth 通道） | 判据是 **`provider` 是否等于 `"DeepAI"`**，别再单看 `system_fingerprint`：真分支也会带 `provider`，且只有 vLLM 后端才给指纹 |
| 号池「指纹突然变差」 | 没完成签到：只拉 `/theos` 不回传 nonce 等于没签到，会落到替身分支 | 等一轮 `EnsureCheckIn` 续签即可；换机器指纹会重新变成冷状态 |
| `403 check_in_refused` | 当天「重启签到」次数用尽（`restarted its check-in too many times today`） | 不要重试、不要把账号标 error：推理仍可用，明天自动恢复；也别再跑签到体检 |
| 上游模型目录默认模型为空 | 字段叫 `default` 不是 `default_model` | 不要加 `default_model` 兼容，会静默拿不到值 |
| 池内账号 `granted=0` | 共用机器指纹，每日额度只发一次 | 不要去掉 `Account.MachineID` 回退逻辑 |
| 长响应被掐断 | 上游心跳 `: keep-alive` 被丢弃 | `streamChat` 要原样透传注释行 |
| 工具调用参数被截断 | 同一事件多行 `data:` 未按 `\n` 拼接 | 用 `deepai.ReadSSE`，不要逐行 `json.Unmarshal` |
| 台账里密码消失 | `Append` 走了 `redactRecord` | 这是设计；密码在 `*-credentials.jsonl` |
| `go test -race` 链接失败（`cannot find 'ld'`） | mingw runtime 只带 `ld.bfd.exe` | 不要求改 PATH；跑 `make race-shim` 补一个同名副本 |
| 非流式请求返回 502「不是合法 JSON」 | 上游**缺省 `stream` 时默认按 SSE 返回** | 必须显式下发 `stream:false`；别把 `stream` 当可选字段略过 |
| 验证点击 0/N（502 / `unexpected EOF` / 超时） | 该端点单次占上游约 13s，并发点会打满 Passenger worker | 不要加并发“修”；保 `verifyClickSlots=2` 与 2s/10s/60s 退避 |
| 新号入池后 `balance=0` 被长期跳过 | 入池瞬间上游尚未下发额度 | 不要把 0 当事实缓存；保留 45s/2m/6min 复探与 15 分钟陈旧判定 |
| 关掉控制台后配额只探了一半 | 后台任务误用 `r.Context()`，页面一关就取消 | 后台任务一律脱离请求 ctx，用独立超时 |
| 这批 `email_verified` 全是 0 | CLI 退出把后台验证 goroutine 一起带走 | 不要删 `-verify-grace`；服务端任务写回要用记录副本 |
| 单号白等 2~3 分钟才失败 | 注册 POST 没有总预算，退避用 `time.Sleep` | 不要放大全局超时；用 45s 预算 + 可取消退避 + 登录兜底 |
| 某条出口挂了导致整批 0 额度 | `No exit node` 是出口故障，不是账号坏 | 不要删号重注册；用 `assign-proxy` 改绑 |

另：`/v1/chat/completions` 的 `reasoning_content` 是网关补的兼容字段，
上游原生只有 `reasoning`；已存在时不会被覆盖。

## 十一、文档索引

| 文件 | 内容 |
| --- | --- |
| `README.md` | 总览、接口、参数、验收基线与已知坑（本文） |
| `docs/lessons.md` | **工程经验沉淀**：十批压测 → 可复用规则 + 改动前检查清单 |
| `docs/recon.md` | 逆向速查：上游端点、请求头、错误语义、复现命令（替代 HAR） |
| `docs/changes.md` | 2026-11 的安全 / 正确性改造记录：根因、决策与验收 |
| `docs/performance.md` | 压测数据与优化方案：单号 E2E、阶段分解、P0/P1/P2 收益测算 |
| `docs/optimization-plan.md` | 首轮扫描的完整优化方案（已全部实施，保留作为设计依据） |
| `docs/optimization-plan-r2.md` | 第二轮扫描（写放大 / 控制台 / 复探）的方案与收益测算 |
| `.env.example` | 全部可配置项与说明 |
| `Makefile` | `make build / test / test-race / lint / cover` |

## 十二、License

MIT。本项目仅复刻本机已登录客户端的调用链路，请遵守 DeepAI 的服务条款。
