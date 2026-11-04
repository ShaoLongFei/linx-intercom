# 灵矽对讲机

ESP32-S3 语音对讲设备及其服务端。目标功能包括语音创建房间、好友配对、邀请确认和在线成员查询。

## 架构

- 设备：ESP32-S3 + ESP-IDF；首次配网使用 BLE BluFi，日常音频通过双向 WebSocket 连接 Go 服务。
- 后端：Go 服务按设备隔离会话，负责音频路由和房间/好友业务。
- 实时媒体：Janus Gateway AudioBridge 负责房间混音。
- 灵矽：虚拟设备通过 WebSocket 接入灵矽；云端 MCP 提供房间和好友工具。

当前已实现健康检查、设备认证 WebSocket、音频/控制帧收发、会话重连和 Linux 虚拟设备模拟器。灵矽、Janus、业务工具和 ESP32 硬件链路仍在开发中。

## 后端

```sh
cd server
DEVICE_TOKENS='{"device-001":"replace-with-device-token"}' go run ./cmd/intercom-server
```

生产联调还需配置 `MCP_API_TOKEN`。绑定过的虚拟灵矽设备配置 `LINX_WS_URL`（优先使用该设备 OTA 返回地址）和 `LINX_DEVICE_TOKENS` JSON 映射。SQLite 文件默认写入 `server/intercom.db`，可通过 `DATABASE_PATH` 修改。服务监听 `LISTEN_ADDR`（默认 `:8080`）。`GET /healthz` 为健康检查，`GET /device/ws` 为设备双向 WebSocket，`GET /mcp` 和 `POST /mcp/message` 为灵矽云端 MCP 服务端点。

设备 WebSocket 请求需带 `Device-Id` 和 `Authorization: Bearer <token>`；服务端只接受 `DEVICE_TOKENS` 中登记的设备。每设备上行音频为二进制 PCM16 单声道帧，控制消息为 JSON 文本帧。设备断线后保留运行会话 60 秒供重连，设备身份和房间/好友数据存于 SQLite。MCP 请求需带服务端配置的 Bearer token 和灵矽提供的 `X-Linx-Device-Id`；工具调用者身份由该设备头映射得出。

运行测试：

```sh
cd server
go test -race ./...
go vet ./...
```

## Linux 虚拟设备

准备 PCM16、16 kHz、单声道 WAV 音频后启动服务，并运行：

```sh
cd server
DEVICE_TOKEN=replace-with-device-token go run ./cmd/intercom-sim \
  -server ws://localhost:8080/device/ws \
  -device-id device-001 \
  -audio-file /path/to/input.wav \
  -reconnect-after 5s \
  -output-file /tmp/device-001-downlink.pcm
```

模拟器以 20 ms 帧实时上传音频，可按指定时间主动重连；服务端下行二进制音频保存为裸 PCM，控制消息输出为结构化日志。模拟器日志不会输出 token 或音频内容。

## ESP32-S3 固件

固件使用 ESP-IDF。进入已配置好的 ESP-IDF 环境后：

```sh
cd firmware/esp32s3
idf.py set-target esp32s3
idf.py build
```

更多架构与部署说明见 `docs/` 和 `deploy/`。本地密钥、设备信息和测试音频不要提交到仓库。
