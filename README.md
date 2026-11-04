# 灵矽对讲机

ESP32-S3 语音对讲设备及其服务端。核心目标是通过语音创建房间、管理好友、邀请确认加入，并查询在线成员。

## 当前技术方向

- 设备：ESP32-S3，ESP-IDF，C/C++；本地唤醒词和音频路由，唤醒词不发送到语聊房。
- 后端：Go，负责房间、好友配对、邀请确认、在线状态和灵矽 MCP 接入。
- 实时媒体：Janus Gateway AudioBridge 作为独立服务运行，ESP32 作为 WebRTC 客户端。
- 灵矽负责语音识别、语义理解、工具调用和语音回复；MCP 承载控制，不承载连续实时音频。

## 目录

```text
firmware/esp32s3/  ESP-IDF 设备工程
server/            Go 业务服务
deploy/            服务部署配置和说明
docs/              产品与技术设计
```

## 开发状态

目前是可继续迭代的项目骨架。后端提供健康检查；设备端工程需安装 ESP-IDF 后编译。Janus 和 WebRTC 媒体接入、好友及房间业务将在后续里程碑实现。

后端本地启动：

```sh
cd server
go run ./cmd/intercom-server
```

访问 `http://localhost:8080/healthz` 检查服务状态。通过 `LISTEN_ADDR` 可修改监听地址。

ESP-IDF 固件构建（需先安装并导出 ESP-IDF 环境）：

```sh
cd firmware/esp32s3
idf.py set-target esp32s3
idf.py build
```

更多设计记录放在 `docs/`；本机配置和测试设备信息放在未跟踪的 `.local/`。
