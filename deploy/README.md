# 部署

## Janus AudioBridge

本地启动独立 Janus 媒体服务：

```sh
docker compose -f deploy/janus-compose.yaml up -d
```

配置开放 TCP `8088`（HTTP WebSocket）和 UDP `10000-10100`（WebRTC ICE/RTP）。生产环境应将 Janus WebSocket 限制为 Go 服务可访问，并通过反向代理提供 TLS/WSS；不要把未启用 HTTPS 的 HTTP transport 直接暴露到公网。部署在 NAT 后方时，需要在 Janus 中设置公网映射地址，或部署 TURN 服务并在 Go 的 ICE server 配置中指定。当前 compose 是联调起点，真实服务器上的防火墙/NAT 地址必须按实际网络校验。

Janus Image、配置路径和配置项需在目标镜像中核对。健康检查调用 Janus HTTP transport 的 `/janus/info`。Go 服务读取 `JANUS_WS_URL`、`JANUS_API_SECRET`、`JANUS_ROOM_ID`、`JANUS_ROOM_PIN`；密钥和房间凭据通过环境变量或平台 Secret 注入，不提交到仓库。

## Go 服务数据

SQLite 数据库默认文件为 `server/intercom.db`，可用 `DATABASE_PATH` 改到持久化卷。停止服务前发送 SIGTERM，服务会执行 HTTP graceful shutdown；备份时先停止写入或使用 SQLite 在线备份能力。日志走 JSON stdout，避免记录音频、Wi-Fi 密码或鉴权 token。

个人机器及测试设备配置放在仓库根目录 `.local/`，不要提交。
