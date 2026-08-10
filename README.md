# Gemini Key Gateway

Gemini API Key 轮询网关：自动切换无效 Key，忠实透传请求/响应体（含 SSE 流式），WebUI 可视化管理。

> <span style="color:red">**⚠️ 警告：本服务持有你全部的 Gemini API Key，且 WebUI/管理 API 仅依赖单一密码认证，严禁将本服务直接暴露到公网！**</span>
>
> <span style="color:red">暴露公网将导致所有 API Key 泄露、被盗刷产生高额费用、管理面板被入侵。请仅在**本机回环地址（127.0.0.1）**、内网或容器虚拟网络内使用，切勿在宿主机或路由器上做公网端口转发。</span>

## 特性

- **Key 轮询**：多 Key 池 round-robin 分配，失败自动切换下一个可用 Key，重试次数可配（`max_retries`）
- **响应分类处理**：
  - `4xx`（429 除外）→ 该 Key **永久失效**，换 Key 重试
  - `429` → 解析响应体 `quotaId` 区分限流类型：
    - **RPD**（含 `PerDay`）→ 锁定该 **Key×Model** 至当日额度刷新点（美国太平洋时间午夜，即北京时间夏令时 15:00 / 冬令时 16:00，自动适配 DST），到点自动赦免
    - **RPM**（含 `PerMinute`）及其他 429 → 该 **Key×Model** 固定冷却 60s 后自动恢复
  - `5xx` → 不重试，原样透传错误
  - 重试耗尽 → **原样透传最后一次上游响应**（状态码/响应头/响应体）
- **忠实流式透传**：SSE 响应逐 chunk 写入并 `Flush`，字节级原样透传，零缓冲聚合、零改写；不干预内容编码
- **并发安全**：每请求独立 goroutine，Key 池互斥锁保护，锁内零网络 I/O，`-race` 全量测试通过
- **WebUI 管理面板**：登录后可视化查看各 Key 状态（可用/失效/禁用/锁定与赦免时刻、请求数、失败数、上次错误），支持运行时添加/删除/启用/禁用 Key，无需重启
- **管理密码**：配置 `admin_password` 即可启用 WebUI 认证；未配置时首次启动自动生成随机密码并打印在日志、写回配置文件
- 单静态二进制，零第三方依赖（`time/tzdata` 内嵌时区数据）

## 快速开始

```bash
# 1. 准备配置（keys 必填）
cp config.example.json config.json
# 编辑 config.json 填入你的 API Key

# 2. 启动
./gemini-key-gateway -config config.json
```

启动日志会打印监听地址与 Key 数量；若未配置 `admin_password`，日志中会给出自动生成的管理密码。

## 配置

```json
{
  "listen": ":8080",
  "upstream": "https://generativelanguage.googleapis.com",
  "max_retries": 5,
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
| `max_retries` | 一次请求最多重试次数（总尝试 = `max_retries` + 1） | `5` |
| `admin_password` | WebUI/管理 API 的认证密码；留空则首次启动自动生成 | 自动生成 |
| `keys` | Gemini API Key 列表（必填） | — |

## Docker 部署

> <span style="color:red">**端口映射只允许绑定 `127.0.0.1`，禁止 `0.0.0.0`（默认即公网可达）！**</span>

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
docker build -t gemini-key-gateway .
docker run -d --name gemini-key-gateway -p 127.0.0.1:8080:8080 \
  -v "$(pwd)/config.json:/app/config.json" \
  gemini-key-gateway
```

### 方式二（推荐）：与 new-api 同一虚拟网络

将网关加入 new-api 所在的 Docker 网络，new-api 通过容器名直接访问，**无需映射任何端口**，不暴露到宿主机：

```bash
# 1. 查看 new-api 所在网络（一般为默认的 new-api 同名网络或自定义网络）
docker network ls
# 以网络名 new-api_default 为例，先创建网关容器
docker run -d --name gemini-key-gateway \
  --network new-api_default \
  -v "$(pwd)/config.json:/app/config.json" \
  gemini-key-gateway
```

若使用 compose，取消 `docker-compose.yml` 中注释的 `networks` 段，并将 `new-api_default` 换成你实际的网络名。

随后在 new-api 后台将网关添加为渠道，BaseURL 填写：

```
http://gemini-key-gateway:8080
```

容器间通过虚拟网络互通，宿主机上没有任何监听端口，最安全。

## 使用

客户端将请求指向网关即可，路径与 Gemini 原生 API 完全一致：

```bash
# 普通生成（无需自带 key，网关自动注入池中可用 Key）
curl -X POST http://127.0.0.1:8080/v1beta/models/gemini-2.0-flash:generateContent \
  -H "Content-Type: application/json" \
  -d '{"contents": [{"parts": [{"text": "你好"}]}]}'

# 流式生成（SSE 字节级透传）
curl -N -X POST "http://127.0.0.1:8080/v1beta/models/gemini-2.0-flash:streamGenerateContent?alt=sse" \
  -H "Content-Type: application/json" \
  -d '{"contents": [{"parts": [{"text": "你好"}]}]}'
```

- 客户端请求中自带的 `key` 查询参数与 `x-goog-api-key` 头会被剥离，统一替换为池中有效 Key
- `GET /v1beta/models` 等模型列表接口同样支持

### WebUI

打开 `http://127.0.0.1:8080/`，输入管理密码登录。面板展示：

- 池总览：总 Key / 可用 / 模型锁定 / 失效 / 禁用
- 各 Key 明细：状态、请求数、失败数、模型锁定（RPD/RPM 类型与解除时刻）、上次错误
- 操作：添加 Key（支持批量，**一行一个**，Ctrl+Enter 提交）、删除 Key、启用 / 禁用
- **增删 Key 自动持久化**：WebUI/API 中添加或删除的 Key 会实时写回 `config.json`，重启不丢失（池内已有的 Key 去重跳过）

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
| `GET` | `/health` | 健康探针：池摘要，免认证 |

认证方式：`Authorization: Bearer <admin_password>`。

## 构建与测试

需要 Go 1.22+（`time/tzdata` 要求 Go 1.15+）：

```bash
go build -trimpath -ldflags "-s -w" -o gemini-key-gateway .   # 静态二进制
go vet ./...
go test -race -count=1 ./...                                  # 全量测试（含并发竞态检测）
```

测试覆盖：Key 轮询、4xx 失效切换、RPD 锁定至刷新点赦免（含 DST 边界）、RPM 60s 冷却、5xx 透传、重试耗尽透传最后响应、SSE 逐字节一致与流式到达、客户端 key 剥离、WebUI 认证、高并发无竞态。

## 文件结构

```
main.go                # 入口：配置加载、路由注册
config.go              # 配置解析、密码生成与持久化
keypool.go             # Key 池状态机：轮询/失效/锁定/冷却/赦免
proxy.go               # 上游转发、响应分类重试、忠实流式透传
webui.go               # WebUI/管理 API/登录认证
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
