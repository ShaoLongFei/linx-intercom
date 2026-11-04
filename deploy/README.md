# 部署

## Janus AudioBridge

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

配置开放 TCP `8088`（HTTP WebSocket）和 UDP `10000-10100`（WebRTC ICE/RTP）。生产环境应将 Janus WebSocket 限制为 Go 服务可访问，并通过反向代理提供 TLS/WSS；不要把未启用 HTTPS 的 HTTP transport 直接暴露到公网。部署在 NAT 后方时，需要在 Janus 中设置公网映射地址，或部署 TURN 服务并在 Go 的 ICE server 配置中指定。当前 compose 是联调起点，真实服务器上的防火墙/NAT 地址必须按实际网络校验。

Janus Image、配置路径和配置项需在目标镜像中核对。健康检查调用 Janus HTTP transport 的 `/janus/info`。Go 服务读取 `JANUS_WS_URL`、`JANUS_API_SECRET` 和可选的 `JANUS_ADMIN_KEY`；业务房间会分配独立的持久数字 ID，并通过 AudioBridge 动态创建，运行时不会共用一个全局房间。密钥通过环境变量或平台 Secret 注入，不提交到仓库。首次运行应在限制访问的测试环境验证 Janus API Secret/Admin Key 与镜像配置一致。

## Go 服务数据

SQLite 数据库默认文件为 `server/intercom.db`，可用 `DATABASE_PATH` 改到持久化卷。停止服务前发送 SIGTERM，服务会执行 HTTP graceful shutdown；备份时先停止写入或使用 SQLite 在线备份能力。日志走 JSON stdout，避免记录音频、Wi-Fi 密码或鉴权 token。

个人机器及测试设备配置放在仓库根目录 `.local/`，不要提交。
