# Scorpio-Balance

Gemini API Key 轮询网关：自动切换无效 Key，忠实透传请求/响应体（含 SSE 流式），WebUI 可视化管理。

> <span style="color:red">**⚠️ 警告：本服务持有你全部的 Gemini API Key，且 WebUI/管理 API 仅依赖单一密码认证，严禁将本服务直接暴露到公网！**</span>
>
> <span style="color:red">暴露公网将导致所有 API Key 泄露、被盗刷产生高额费用、管理面板被入侵。请仅在**本机回环地址（127.0.0.1）**、内网或容器虚拟网络内使用，切勿在宿主机或路由器上做公网端口转发。</span>

## 特性

- **Key 轮询**：多 Key 池 round-robin 分配，失败自动切换下一个可用 Key，重试次数可配（`max_retries`）
- **响应分类处理**：
  - `4xx`（429 除外）→ 该 Key **永久失效**，换 Key 重试；`400` 除外（请求体/请求参数本身有问题，换 Key 无济于事）→ **不标记 Key、不重试，直接透传**，由客户端自行修正请求
  - `429` → 解析响应体 `quotaId` 区分限流类型：
    - **RPD**（含 `PerDay`）→ 锁定该 **Key×Model** 至当日额度刷新点（美国太平洋时间午夜，即北京时间夏令时 15:00 / 冬令时 16:00，自动适配 DST），到点自动赦免
    - **TPM**（含 `PerMinute`+`Tokens`，如 `GenerateContentInputTokensPerModelPerMinute-*`）→ 该 **Key×Model** 固定冷却 60s；请求自身的 token 数已超限，**不换 Key 重试**，直接透传 429 给客户端，避免把所有 Key 逐一误锁
    - **RPM**（含 `PerMinute`）及其他 429 → 该 **Key×Model** 固定冷却 60s 后自动恢复，换 Key 重试
  - `5xx` → 不重试，原样透传错误
  - **网络错误/响应头超时**（`request_timeout` 内上游无响应，如挂起、满载排队）→ 不重试、不标记 Key，网关直接回 `503`（`The Gemini API did not provide any response before timing out.`），由下游自行重试/降级
  - 重试耗尽 → **原样透传最后一次上游响应**（状态码/响应头/响应体）
- **忠实流式透传**：SSE 响应逐 chunk 写入并 `Flush`，字节级原样透传，零缓冲聚合、零改写；不干预内容编码
- **安全拦截自动重试（防误报，默认关闭）**：检测到 `generateContent`/`streamGenerateContent` 响应被安全机制拦截（`promptFeedback.blockReason` 非空或候选 `finishReason` 为 SAFETY 等）时，自动在请求体 `contents` 末尾追加一条 `user: EOF` 消息，然后像普通重试一样从 Key 池中 Pick 下一个可用 Key 重试，利用上游「只检查最后一条 user 消息」的特性减少正常对话被误报拦截（需显式开启 `block_retry: true` 并设 `max_block_retries ≥ 1`）
- **并发安全**：每请求独立 goroutine，Key 池互斥锁保护，锁内零网络 I/O，`-race` 全量测试通过
- **WebUI 管理面板**：登录后可视化查看各 Key 状态（可用/失效/禁用/锁定与赦免时刻、请求数、失败数、上次错误），支持运行时添加/删除/启用/禁用 Key，无需重启
- **管理密码**：配置 `admin_password` 即可启用 WebUI 认证；未配置时首次启动自动生成随机密码并打印在日志、写回配置文件
- **代理转发鉴权**：`/v1beta` 代理默认要求携带网关密钥（默认沿用 `admin_password`），支持 `x-goog-api-key` 头或 `Authorization: Bearer`；new-api 渠道把密钥填在「密钥」栏即可。配置 `"proxy_auth": false` 可关闭（不建议，尤其公网）
- 单静态二进制，零第三方依赖（`time/tzdata` 内嵌时区数据）

## 快速开始

```bash
# 1. 准备配置（keys 必填）
cp config.example.json config.json
# 编辑 config.json 填入你的 API Key

# 2. 启动
./scorpio-balance -config config.json
```

启动日志会打印监听地址与 Key 数量；若未配置 `admin_password`，日志中会给出自动生成的管理密码。该密码同时用于 WebUI 登录与 `/v1beta` 代理转发鉴权（默认开启）。

## 配置

```json
{
  "listen": ":8080",
  "upstream": "https://generativelanguage.googleapis.com",
  "max_retries": 5,
  "request_timeout": 30,
  "proxy_auth": true,
  "admin_password": "your-password",
  "keys": [
    "AIzaSyA...your-first-key...",
    "AIzaSyB...your-second-key..."
  ]
}
```

| 字段 | 说明 | 默认 |
|---|---|---|
| `listen` | 监听地址 | `:8080` |
| `upstream` | Gemini API 上游地址 | `https://generativelanguage.googleapis.com` |
| `max_retries` | 一次请求最多重试次数（总尝试 = `max_retries` + 1）；显式填 `0` 表示不重试 | `5` |
| `request_timeout` | 上游响应头等待超时（秒）。上游未在超时内发出任何响应头（如挂起/满载排队）则网关不重试，直接回 503（`The Gemini API did not provide any response before timing out.`），重试交给下游 | `30` |
| `block_retry` | 安全拦截自动重试开关；省略字段时默认关闭，设 `true` 开启 | `false` |
| `max_block_retries` | 开启 `block_retry` 后，单次请求因安全拦截最多追加「EOF」消息并重试的次数（占用 `max_retries` 额度）；`<= 0` 表示不重试 | `0` |
| `block_retry_mode` | 拦截判定模式：`stream` = 只检查流式响应首块（SSE 首事件 / JSON 数组首元素），未拦截立即透传，保持流式实时性（默认，流中途被截断不再重试）；`full` = 完整缓冲整个响应（能发现流中途截断，但流式首字节延迟） | `stream` |
| `proxy_auth` | 代理转发鉴权开关；省略时默认开启。开启后 `/v1beta` 请求必须携带正确密钥（`x-goog-api-key` 头或 `Authorization: Bearer <admin_password>`），否则返回 401 | `true` |
| `admin_password` | WebUI/管理 API 的认证密码，同时作为代理转发鉴权的默认密钥；留空则首次启动自动生成 | 自动生成 |
| `keys` | Gemini API Key 列表（必填） | — |

## 安全拦截自动重试（防误报）

Gemini API 存在一个已知 Bug：安全过滤机制误把正常对话判为违规，导致 `generateContent` / `streamGenerateContent` 返回空回复（HTTP 200，但响应体含 `promptFeedback.blockReason`）。Google 工程师建议的缓解方案是——上游实际只会检查**最后一条 user 消息**，前面的历史并不参与本次拦截判定。因此在历史末尾追加一条占位 user 消息后重试，给模型更多思考空间，从而大幅减少误报：

```
user：早上好                        ← 原请求最后一条
user：EOF        ← 网关自动追加（成为新的「最后一条 user 消息」）
```

网关在检测到拦截后，会在请求体 `contents` 末尾自动追加上述一条 user 消息，并像普通重试一样从 Key 池中 Pick 下一个可用 Key 重试，无需客户端改动。检测兼容非流式 JSON、流式 SSE、JSON 数组与 gzip 压缩响应。

> 该功能**默认关闭**（`block_retry: false`、`max_block_retries: 0`）。如需启用，在 `config.json` 中设置 `"block_retry": true` 并将 `"max_block_retries"` 设为 `1`（或更大）。

> **注意**
>
> - 安全拦截重试**占用一次 `max_retries` 重试额度**：检测到拦截后，网关回到普通重试循环、从 Key 池 Pick 下一个可用 Key；若 `max_retries` 已用完则直接透传最后一次响应。
> - 这只是一种**减少误报**的缓解手段，并非真正绕过安全机制：若历史里确实存在违规内容，API 层面仍可能拦截，追加对话只是给模型更多空间，不改变安全底线。
> - 拦截检测默认采用 `block_retry_mode: "stream"`：网关只缓冲流式响应的首块（SSE 首事件 / JSON 数组首元素）做拦截判定，未拦截立即透传，首字节延迟接近零缓冲水平；代价是**流中途被安全截断不会触发重试**（可接受「输出到一半被截断」时用这个折中模式）。若你更看重拦截覆盖面、需要发现流中途的截断，可改用 `"block_retry_mode": "full"`：content 生成端点的 2xx 响应需先整体读入内存以判定是否被拦截，未拦截时再透传，因此**流式响应的首个字节会延迟到上游生成完成后才到达客户端**。完全不需要拦截重试时保持关闭（默认）即可。
> - 对 agent 框架的 **tools loop（函数调用）** 场景，追加「EOF」可能打断模型的多步工具调用流程，建议按需在纯文本对话场景下开启。

## Docker 部署

> <span style="color:red">**端口映射只允许绑定 `127.0.0.1`，禁止 `0.0.0.0`（默认即公网可达）！**</span>

### 方式〇：直接使用 GitHub Actions 自动构建的镜像（免本地编译）

仓库已配置 GitHub Actions（`.github/workflows/build-image.yml`）：推送 `main`/`beta` 分支、打 `v*` 标签或在 Actions 页面手动触发时，GitHub 会自动运行测试并构建 Docker 镜像，推送到 **GHCR**（GitHub 容器仓库）。

- 镜像地址：`ghcr.io/2ertwo6/scorpio-balance`
- 标签规则：`main` 推送 → `latest` + `main` + `sha-<commit>`；`beta` 推送 → `beta` + `sha-<commit>`；`v1.2.3` 标签 → `1.2.3`、`1.2`、`1` + `latest`
- 多架构：`linux/amd64`（常见服务器）、`linux/arm64`（NAS / 树莓派）

```bash
docker pull ghcr.io/2ertwo6/scorpio-balance:latest
docker run -d --name scorpio-balance -p 127.0.0.1:8080:8080 \
  -v "$(pwd)/config.json:/app/config.json" \
  ghcr.io/2ertwo6/scorpio-balance:latest
```

> 首次推送后镜像默认为私有，需到仓库 **Packages** 页（<https://github.com/2erTwo6/Scorpio-Balance/pkgs/container/scorpio-balance>）将可见性改为 Public，其他机器才能直接 `docker pull`。

### 方式一：仅本机访问

```bash
# 1. 准备配置（keys 必填）
cp config.example.json config.json
# 编辑 config.json 填入你的 API Key

# 2. 构建并启动
docker compose up -d --build
```

- 配置通过 volume 挂载（`./config.json:/app/config.json`），运行时改密码等操作会持久化回宿主机文件
- 端口已映射到 `127.0.0.1:8080:8080`，仅宿主机本机可访问，请勿修改为 `8080:8080`
- 单独使用镜像：

```bash
docker build -t scorpio-balance .
docker run -d --name scorpio-balance -p 127.0.0.1:8080:8080 \
  -v "$(pwd)/config.json:/app/config.json" \
  scorpio-balance
```

### 方式二（推荐）：与 new-api 同一虚拟网络

将网关加入 new-api 所在的 Docker 网络，new-api 通过容器名直接访问，**无需映射任何端口**，不暴露到宿主机：

```bash
# 1. 查看 new-api 所在网络（一般为默认的 new-api 同名网络或自定义网络）
docker network ls
# 以网络名 new-api_default 为例，先创建网关容器
docker run -d --name scorpio-balance \
  --network new-api_default \
  -v "$(pwd)/config.json:/app/config.json" \
  scorpio-balance
```

若使用 compose，取消 `docker-compose.yml` 中注释的 `networks` 段，并将 `new-api_default` 换成你实际的网络名。

随后在 new-api 后台将网关添加为渠道，BaseURL 填写：

```
http://scorpio-balance:8080
```

渠道「密钥」栏填写网关的 `admin_password`（即代理转发鉴权密钥）。new-api 会以 `x-goog-api-key` 请求头把它传给网关，网关校验通过后才会转发并自动替换为池中的 Gemini Key；填错时网关返回 401，渠道测试会失败。

容器间通过虚拟网络互通，宿主机上没有任何监听端口，最安全。

## 使用

客户端将请求指向网关即可，路径与 Gemini 原生 API 完全一致：

```bash
# 普通生成（需携带网关密钥 = admin_password；鉴权通过后由网关自动注入池中可用 Key）
curl -X POST http://127.0.0.1:8080/v1beta/models/gemini-2.0-flash:generateContent \
  -H "Content-Type: application/json" \
  -H "x-goog-api-key: your-admin-password" \
  -d '{"contents": [{"parts": [{"text": "你好"}]}]}'

# 流式生成（SSE 字节级透传）
curl -N -X POST "http://127.0.0.1:8080/v1beta/models/gemini-2.0-flash:streamGenerateContent?alt=sse" \
  -H "Content-Type: application/json" \
  -H "x-goog-api-key: your-admin-password" \
  -d '{"contents": [{"parts": [{"text": "你好"}]}]}'
```

- 网关密钥校验通过后，客户端请求中自带的 `key` 查询参数与 `x-goog-api-key` 头会被剥离，统一替换为池中有效 Key（鉴权密钥不会泄露给上游）
- 也可用 `Authorization: Bearer your-admin-password` 代替 `x-goog-api-key` 头
- `GET /v1beta/models` 等模型列表接口同样支持（同样需要鉴权）

### WebUI

打开 `http://127.0.0.1:8080/`，输入管理密码登录。面板分为「Key 池」和「设置」两个页签：

**Key 池**

- 池总览：总 Key / 可用 / 模型锁定 / 失效 / 禁用
- 各 Key 明细：状态、请求数、失败数、模型锁定（RPD/RPM 类型与解除时刻）、上次错误
- 操作：添加 Key（支持批量，**一行一个**，Ctrl+Enter 提交）、删除 Key、启用 / 禁用
- **增删 Key 自动持久化**：WebUI/API 中添加或删除的 Key 会实时写回 `config.json`，重启不丢失（池内已有的 Key 去重跳过）

**设置**

- 所有可配置项均可在 WebUI 中调整：`listen`、`upstream`、`max_retries`、`request_timeout`、`block_retry`、`max_block_retries`、`block_retry_mode`、`proxy_auth`、`admin_password`
- 除 `listen`（监听地址）需**重启进程**生效外，其余配置保存后**立即生效**：上游地址、重试次数、请求超时、安全拦截重试、代理鉴权、管理密码都会热更新
- 修改 `admin_password` 后当前浏览器会话会自动使用新密码重新认证，无需重新登录
- 提供「重启进程…」按钮：通过 `POST /api/restart` 让进程重新执行自身（`syscall.Exec`，PID 不变，Docker 容器 PID 1 场景同样适用），使 `listen` 等需要重启的配置生效

#### 外部访问：临时 SSH 隧道（推荐）

需要从其他设备访问 WebUI 时，**推荐使用临时 SSH 隧道**，而不是对宿主机做公网端口转发：

```bash
# 在本地设备（如笔记本）上执行，将远程 8080 映射到本地 18080
ssh -L 18080:127.0.0.1:8080 user@server-ip -N
# 随后浏览器打开 http://127.0.0.1:18080/ 即可
```

- 流量全程 SSH 加密，网关本身不向公网开放任何端口，无 Key 泄露面
- 用完即断（`Ctrl+C` 结束隧道），属于临时性访问，不必为 WebUI 单独配置 VPN/防火墙规则
- 若网关跑在容器里且未映射端口（与 new-api 同网络的方式二），可将 `127.0.0.1:8080` 换成 `容器IP:8080`，或映射到宿主机回环地址后使用本命令

### 管理 API

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/api/login` | 密码登录，返回 token（无认证） |
| `GET` | `/api/pool` | 池状态明细（需认证） |
| `POST` | `/api/keys` | 添加 Key（需认证）：单个 `{"key": "..."}` 或批量 `{"keys": ["...", "..."]}`，自动去重；返回 `{"added": N, "ids": [...], "persisted": bool}` |
| `DELETE` | `/api/keys/{id}` | 删除 Key（需认证），删除结果同样持久化 |
| `POST` | `/api/keys/{id}/state` | 启用/禁用，body `{"state": "enabled"\|"disabled"}`（需认证） |
| `GET` | `/api/config` | 读取当前运行配置（需认证）；`admin_password` 不返回明文，只返回 `admin_password_set: true/false` |
| `PUT` | `/api/config` | 更新配置（需认证），body 中出现的字段才会更新；保存成功后写回 `config.json` 并热更新可即时生效项。响应 `{"ok": true, "applied": [...], "restart_required": [...], "password_changed": bool, "persisted": true}` |
| `POST` | `/api/restart` | 重启进程（需认证）：先返回 `{"ok": "true"}`，约 200ms 后执行 `syscall.Exec` 重新启动自身，用于使 `listen` 生效 |
| `GET` | `/health` | 健康探针：池摘要，免认证 |

认证方式：`Authorization: Bearer <admin_password>`。

## 构建与测试

需要 Go 1.22+（`time/tzdata` 要求 Go 1.15+）：

```bash
go build -trimpath -ldflags "-s -w" -o scorpio-balance .   # 静态二进制
go vet ./...
go test -race -count=1 ./...                                  # 全量测试（含并发竞态检测）
```

测试覆盖：Key 轮询、4xx 失效切换、400 透传不重试不标记、RPD 锁定至刷新点赦免（含 DST 边界）、RPM 60s 冷却、TPM 锁定并透传不重试、5xx 透传、重试耗尽透传最后响应、响应头超时网关自回 503 且不重试、SSE 逐字节一致与流式到达、客户端 key 剥离、WebUI 认证、配置读取/保存/热更新/非法值拒绝、空白批量 Key 不越界、高并发无竞态。

## 文件结构

```
main.go                # 入口：配置加载、路由注册
config.go              # 配置解析、密码生成与持久化
settings.go            # 运行配置读取/保存/热更新（WebUI 设置页后端）
keypool.go             # Key 池状态机：轮询/失效/锁定/冷却/赦免
proxy.go               # 上游转发、响应分类重试、忠实流式透传
webui.go               # WebUI/管理 API/登录认证/动态密码门禁
restart_unix.go        # 进程自重启（syscall.Exec，Unix）
restart_other.go       # 非 Unix 平台重启占位
web/index.html         # 内嵌管理面板（go:embed）
config.example.json    # 配置示例
*_test.go              # 单元与集成测试
```

## 说明

- RPD 额度刷新时刻计算等价于：夏令时北京时间 15:00 / 冬令时 16:00（基于美国太平洋时间午夜，自动适配 DST）
- 429 响应中的 `retryDelay` 字段不可信，已忽略
- 不同模型的 RPD 配额相互独立，锁定粒度精确到 Key×Model，不影响该 Key 其他模型
- 管理密码存储于 `config.json`（明文），请勿提交该文件到版本库
- WebUI/API 增删 Key 会写回 `config.json`（保留其余字段）；配置文件的写权限因此是运行前提，容器部署时请确保挂载卷可写
