# 灵矽对讲机

ESP32-S3 语音对讲设备及其服务端。目标功能包括语音创建房间、好友配对、邀请确认和在线成员查询。

## 架构

- 设备：ESP32-S3 + ESP-IDF；首次配网使用 BLE BluFi，日常音频通过双向 WebSocket 连接 Go 服务。
- 后端：Go 服务按设备隔离会话，负责音频路由和房间/好友业务。
- 实时媒体：Janus Gateway AudioBridge 负责房间混音。
- 灵矽：Go 为每个 ESP32 会话请求 OTA 配置，待绑定时通过设备扬声器播报激活码，绑定后用独立 URL/token 建立虚拟设备 WebSocket；云端 MCP 提供房间和好友工具。

当前已实现健康检查、设备认证 WebSocket、音频/控制帧收发、会话重连、Linux 虚拟设备模拟器、SQLite 房间/好友数据、OTA 自动绑定状态机、灵矽 WebSocket/MCP 工具、动态 Janus 房间、服务端 sherpa-onnx 唤醒检测接入，以及 ESP32-S3 BluFi/I2S/WebSocket 固件。运行 `bash deploy/prepare-kws.sh` 可下载并配置“灵矽灵矽”中文 KWS 模型；模型权重留在 Git 忽略目录。OTA 自动绑定和普通话合成尚未用真实灵矽账号与实体硬件验证；Compose smoke test 与 fake peer 测试不能替代真实 Janus 双设备媒体、BluFi 手机兼容性或硬件音频联调。

## 后端

```sh
cd server
DEVICE_TOKENS='{"device-001":"replace-with-device-token"}' go run ./cmd/intercom-server
```

生产部署配置 `DEVICE_TOKENS` 和 `MCP_API_TOKEN`。每个设备通过 `device_info` 报告 OTA 元数据；Go 动态获取绑定状态与 Linx 凭证，不需要静态 `LINX_WS_URL` 或 `LINX_DEVICE_TOKENS`。本地直接运行时需安装 `espeak-ng`，Docker 镜像已包含。SQLite 文件默认写入 `server/intercom.db`，可通过 `DATABASE_PATH` 修改。服务监听 `LISTEN_ADDR`（默认 `:8080`）。`GET /healthz` 为健康检查，`GET /device/ws` 为设备双向 WebSocket，`GET /mcp` 和 `POST /mcp/message` 为灵矽云端 MCP 服务端点。

设备 WebSocket 请求需带 `Device-Id` 和 `Authorization: Bearer <token>`；服务端只接受 `DEVICE_TOKENS` 中登记的设备。每设备上行音频为二进制 PCM16 单声道帧，控制消息为 JSON 文本帧。设备断线后保留运行会话 60 秒供重连，设备身份、稳定 UUID v4 灵矽 `Client-Id`、房间和好友数据存于 SQLite。MCP 请求需带服务端配置的 Bearer token 和灵矽提供的 `X-Linx-Device-Id`；工具调用者身份由该设备头映射得出。

服务为排障记录完整 Wi-Fi SSID、OTA 请求/响应、激活码和 Linx token。音频日志只记录方向、帧长度、帧数和会话信息，不记录 PCM、WAV 或其他可还原音频的内容。仍不记录设备到 Go 的 bearer token、MCP API token 或 Wi-Fi 密码。Linx token 只保留在进程和设备会话内存中，但 OTA 响应与连接日志会包含其原始值。

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
