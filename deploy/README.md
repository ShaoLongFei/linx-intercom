# 部署

## 本地 Smoke Test

已安装 Docker Compose、Go 和 Python 3 后，在仓库根目录运行：

```sh
bash deploy/smoke-test.sh
```

脚本会启动独立命名的 Compose 项目，检查 `/healthz`，生成短 WAV 并通过 Linux 模拟器验证设备鉴权、音频上传和 60 秒宽限期内重连，再检查 SIGTERM 正常退出，结束后删除该 smoke 项目的容器及数据卷。它只读取 `deploy/server.env.example`，覆盖为临时测试身份，禁用真实灵矽账号和 KWS 模型。可通过 `INTERCOM_SMOKE_HTTP_PORT`、`JANUS_SMOKE_HTTP_PORT` 和 `JANUS_SMOKE_WS_PORT` 更改测试端口；Janus 的 UDP 10000–10100 仍需空闲。

## Janus AudioBridge

当前 Compose 使用 Docker Hub 上可用的 `canyan/janus-gateway:1.1.4`，镜像仅提供 `linux/amd64`。该镜像 README 的配置示例采用单文件挂载；不要覆盖整个 `/usr/local/etc/janus` 目录。

本地启动独立 Janus 媒体服务：

```sh
docker compose -f deploy/janus-compose.yaml up -d
```

完整 Go + Janus 部署：复制 `server.env.example` 为 `.env`，生成每台设备独立的 `DEVICE_TOKENS`、灵矽 `LINX_DEVICE_TOKENS` 和高熵 `MCP_API_TOKEN`，然后运行：

```sh
cp deploy/server.env.example deploy/.env
docker compose --env-file deploy/.env -f deploy/compose.yaml up --build -d
```

SQLite 存在 `intercom-data` named volume 中。升级前备份该卷中的 `intercom.db`；不要删除数据卷。Compose health check 通过服务自身的 `/healthz` 检查进程就绪。

### 唤醒词模型

服务端使用 sherpa-onnx 流式 KWS。Linux amd64/arm64 镜像内含对应原生运行库；本地放入模型文件到 `deploy/models/kws/`，该目录内容默认被 Git 忽略。至少准备 encoder、decoder、joiner、tokens 和关键词文件，并在 `.env` 中设置容器内路径，例如：

```dotenv
KWS_ENCODER=/opt/kws/encoder.onnx
KWS_DECODER=/opt/kws/decoder.onnx
KWS_JOINER=/opt/kws/joiner.onnx
KWS_TOKENS=/opt/kws/tokens.txt
KWS_KEYWORDS_FILE=/opt/kws/keywords.txt
KWS_NUM_THREADS=1
```

五个文件路径必须一起配置；全部留空时服务会明确记录唤醒检测已禁用，部分配置或模型加载失败会阻止服务启动。每个在线物理设备独立加载一份模型与流状态，CPU/内存会随在线设备数增加；上线前需用目标模型测量单实例资源占用。当前默认关键词分数为 3.0、阈值为 0.1。回看缓存约为 1 秒（50 个 20 ms 帧），普通对讲会相应延迟；尚未用目标唤醒短语的录音验证误触发、漏检和实际延迟。

配置开放 TCP `8088`（HTTP WebSocket）和 UDP `10000-10100`（WebRTC ICE/RTP）。生产环境应将 Janus WebSocket 限制为 Go 服务可访问，并通过反向代理提供 TLS/WSS；不要把未启用 HTTPS 的 HTTP transport 直接暴露到公网。部署在 NAT 后方时，需要在 Janus 中设置公网映射地址，或部署 TURN 服务并在 Go 的 ICE server 配置中指定。当前 compose 是联调起点，真实服务器上的防火墙/NAT 地址必须按实际网络校验。

Janus Image、配置路径和配置项需在目标镜像中核对。健康检查调用 Janus HTTP transport 的 `/janus/info`。Go 服务读取 `JANUS_WS_URL`、`JANUS_API_SECRET` 和可选的 `JANUS_ADMIN_KEY`；业务房间会分配独立的持久数字 ID，并通过 AudioBridge 动态创建，运行时不会共用一个全局房间。密钥通过环境变量或平台 Secret 注入，不提交到仓库。首次运行应在限制访问的测试环境验证 Janus API Secret/Admin Key 与镜像配置一致。

Compose 只覆盖镜像内的 `janus.jcfg` 主配置文件，保留镜像提供的 HTTP/WebSocket transport 和 AudioBridge 插件配置；不要把仅含主配置的 `deploy/janus/` 整个目录挂载到 `/usr/local/etc/janus`，否则会遮住其余模块配置。

## Go 服务数据

SQLite 数据库默认文件为 `server/intercom.db`，可用 `DATABASE_PATH` 改到持久化卷。停止服务前发送 SIGTERM，服务会执行 HTTP graceful shutdown；备份时先停止写入或使用 SQLite 在线备份能力。日志走 JSON stdout，避免记录音频、Wi-Fi 密码或鉴权 token。

个人机器及测试设备配置放在仓库根目录 `.local/`，不要提交。

## 灵矽云端 MCP

按[灵矽云端 MCP 文档](https://linx.qiniu.com/docs/xrobot/mcp/software-mcp)，在智能体配置中登记 `https://你的域名/mcp`。平台请求必须携带 `Authorization: Bearer <MCP_API_TOKEN>`，并提供与本服务已登记设备一致的 `X-Linx-Device-Id`。当前实现按设备建立独立 SSE 会话，因此 GET 和后续 POST 都需要该设备头；实际平台是否为首次 GET 提供该头还需真实账号联调确认。

`GET /mcp` 返回 SSE `endpoint` 事件，地址为 `/mcp/message?sessionid=...`；客户端向该地址发送 JSON-RPC 请求，POST 返回 202，实际响应通过原 SSE 连接的 `message` 事件返回。标准 `initialize`、初始化通知、`tools/list`、`tools/call` 和 `ping` 由官方 Go MCP SDK 处理。反向代理需允许长连接、关闭 SSE 响应缓冲、保留查询参数和请求头；相对 endpoint 地址会沿用客户端 HTTPS origin。

每条 SSE 会话的身份在 GET 鉴权后固定，POST 不允许换成另一台设备。断开后删除临时会话，旧 POST 地址返回 404；重连时重新建立 MCP 传输会话，SQLite 中的用户、好友和房间不受影响。不带 `sessionid` 的直接 POST 仍可用于本地业务诊断，它不是标准 SSE 传输。
