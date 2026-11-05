# 灵矽 OTA 自动绑定实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Go 服务为每台 ESP32-S3 自动请求灵矽 OTA 配置，在设备未绑定时通过扬声器播报六位码并轮询绑定状态，绑定后用 OTA 返回的 URL/token 建立独立灵矽会话。

**Architecture:** 固件在设备 WebSocket 建立后发送 OTA 所需的 `device_info`；Go 校验其设备身份并保存在该设备会话内存中。会话级 OTA 状态机负责请求 OTA、播报激活码、检查绑定、刷新凭证和管理灵矽连接；`espeak-ng` 只负责合成激活提示，现有设备 PCM 下行负责播放。

**Tech Stack:** Go 1.26、`net/http`、`encoding/json`、`os/exec`、ESP-IDF 5.3.x、ESP32-S3 BluFi/I2S/WebSocket、灵矽 OTA/WebSocket。

**Spec:** `docs/superpowers/specs/2026-11-05-linx-ota-auto-binding-design.md`

## 全局约束

- `Device-Id` 和 OTA `mac_address` 必须使用同一个 Wi-Fi STA MAC；OTA 返回的 Linx token 只进入 Go 设备会话内存，不发给 ESP32，也不写入 SQLite。
- 当前未预注册序列号的设备使用 `Activation-Version: 1`；提供有效预注册序列号时使用 `Activation-Version: 2` 和 `Serial-Number` 请求头。
- 当前项目按用户确认记录完整原始 Wi-Fi SSID、OTA 请求/响应与 token、激活码，以及设备 WebSocket 上下行 PCM 音频；二进制音频按帧编码为完整 Base64 日志字段，不截断。不记录设备到 Go 的认证 token、MCP API token 或 Wi-Fi 密码。
- 所有轮询、重试和音频合成都必须响应会话取消；临时 OTA/Linx 错误不得结束仍有效的 ESP32 会话。
- 激活码首次收到时播报，此后每 60 秒播报；绑定轮询从 5 秒开始，最长间隔 30 秒；普通失败退避从 1 秒开始，最长间隔 30 秒。
- 设备音频保持 PCM16、16 kHz、单声道、每帧 640 字节/20 ms；播放使用现有有序设备下行队列。
- 生产 OTA 地址固定为 `https://xrobo.qiniuapi.com/v1/ota/`；设备 URL/token 从 OTA 返回值获取，不读取静态 `LINX_DEVICE_TOKENS` 或 `LINX_WS_URL`。
- 保留 `DEVICE_TOKENS`、`MCP_API_TOKEN`、KWS 和可选 Janus 配置；不得记录 Wi-Fi 密码或设备到 Go/MCP 凭证。
- 所有 Go 行为变更先写失败测试、确认失败原因，再实现；完成每个里程碑后运行对应测试并提交。

## Review Focus

- 伪造或残缺的 `device_info`（身份不匹配、缺少必需板卡/固件字段、版本与序列号矛盾）必须在 OTA 前拒绝；Task 2 覆盖。
- OTA 返回 HTTP 200 业务错误、字段缺失或不完整 URL/token 时，不误判绑定成功；Tasks 1 和 4 覆盖。
- `/ota/activate` 返回 202、success、异常正文、HTTP 错误或 session 取消时，轮询和 OTA 刷新必须符合退避/取消规则；Tasks 1 和 4 覆盖。
- `espeak-ng` 超时、退出失败、输出超限、非 PCM16 mono WAV 或非预期采样率时，不崩溃、不发送损坏帧；Task 3 覆盖。
- 两台设备并行或一台重连时，激活码/token/音频/取消信号不串会话；日志中的每方向 Base64 音频约产生 3.68 GB/日/设备，部署文档必须明确说明；Task 4/5 覆盖。

---

### Task 1: OTA HTTP 协议客户端

**Files:**
- Create: `server/internal/ota/client.go`
- Create: `server/internal/ota/client_test.go`

**Interfaces:**
- `ota.DeviceInfo` 携带 `activation_version`、可选 `serial_number`、application、board、MAC、chip、flash 和 partition 元数据。
- `ota.Binding` 返回 `ActivationCode`、`WebSocketURL` 和 `WebSocketToken`；只有 code 或 URL/token 成对存在时才是有效业务状态。
- 设备 JSON 固定为 `{"type":"device_info","device_info":{"activation_version":1,"report":{"application":{},"board":{},"mac_address":"..."}}}`；`report` 其余字段遵循官方 OTA JSON 名称，`uuid` 由 Go 填入请求体。
- OTA 上报 JSON 包含样例中的顶层 `version: 0`、`uuid`、`application`、`board`、`mac_address`、`chip_model_name`、`flash_size`、`chip_info` 和 `partition_table`；HTTP 请求超时 15 秒，响应体最多读取 1 MiB。
- `(*ota.Client).Report(ctx, deviceID, clientID, userAgent string, info DeviceInfo) (Binding, error)` 发送 OTA 上报并验证返回体。
- `(*ota.Client).CheckActivation(ctx, deviceID string) (bool, error)` 在 202 时返回未绑定，在 200 文本 `success` 时返回已绑定。
- 生产构造器固定官方 OTA 地址；包内测试构造器接受 `httptest.Server` 地址和 HTTP client。

- [x] 编写 `httptest`，验证 `Report` 发送 POST、`Activation-Version`、`Device-Id`、`Client-Id`、`User-Agent`、`Accept-Language: zh-CN`，并验证 JSON 中 MAC、UUID、固件和板卡字段与输入一致。
- [x] 编写响应测试：未绑定激活码响应返回 pending；完整 URL/token 响应返回 bound；URL/token 不成对、无 code/token、无效 JSON 和 HTTP 200 `error` 均返回协议错误。
- [x] 编写激活检查测试：202 返回 false、200 `success` 返回 true、HTTP 200 的 JSON 错误和非预期正文报错、HTTP 非 2xx 返回状态错误。
- [x] 运行 `cd server && go test ./internal/ota -run 'TestReport|TestCheckActivation' -count=1`，确认测试因包/行为尚未实现而失败。
- [x] 实现 15 秒 HTTP 请求超时、1 MiB 响应体上限、context-aware HTTP 请求、版本/序列号/MAC/必要字段校验和 OTA 请求/响应原始值结构化日志；请求正文中的 `uuid` 由持久 `Client-Id` 填入，`mac_address` 取已认证 `Device-Id`，`User-Agent` 取 application name/version。
- [x] 重跑 `cd server && go test ./internal/ota -count=1`，确认全部协议测试通过。
- [x] 运行 `cd server && go test ./...`，确认现有 Go 套件仍通过。
- [x] 提交：`git add server/internal/ota && git commit -m "feat: add Linx OTA client"`。

### Task 2: 设备元数据消息与 ESP32 上报

**Files:**
- Modify: `server/internal/device/protocol.go`
- Modify: `server/internal/device/websocket.go`
- Modify: `server/internal/device/session.go`
- Modify: `server/internal/device/session_test.go`
- Modify: `firmware/esp32s3/main/backend_client.c`
- Modify: `firmware/esp32s3/main/backend_client.h`
- Modify: `firmware/esp32s3/main/Kconfig.projbuild`
- Modify: `firmware/esp32s3/main/CMakeLists.txt`
- Modify: `firmware/esp32s3/README.md`

**Interfaces:**
- 设备文本消息为 `{"type":"device_info","device_info":{...}}`；内层 JSON 使用 Task 1 的 `ota.DeviceInfo` 字段格式。
- `Session.DeviceInfo` 的类型为 `chan ota.DeviceInfo`，容量为 1；新元数据到达时替换待处理的旧值。设备重连后重发元数据只更新同一 session，不创建第二个 OTA worker。
- WebSocket 层校验完整结构、`device_info.mac_address == Device-Id` 和 activation version/serial 组合。无效消息只记录错误并丢弃，不断开设备或传给 Linx。

- [x] 先为 `device_info` 添加解码和验证测试：合法 MAC/版本被接受；MAC 不匹配、缺失 board SSID、缺失固件版本/hash、版本 2 无序列号、版本 1 含序列号均被拒绝。
- [x] 运行 `cd server && go test ./internal/device -run 'TestDecodeDeviceInfo|TestDeviceInfo' -count=1`，确认新测试在尚无元数据处理时失败。
- [x] 在 `ControlMessage` 和设备会话中加入类型化元数据通道；WebSocket 层校验后写入该通道并跳过 `ControlIn` 转发，避免将 OTA 元数据发给 Linx。
- [x] 增加 WebSocket 测试，验证 MAC 不匹配的已认证设备仍保持连接但元数据不会到达 OTA 通道，合法重发会更新同一 session。
- [x] 固件在 WebSocket connected 事件中发送 `device_info`：从 ESP-IDF app description 读取 application/version/compile time/IDF version/ELF SHA-256，从 STA 接口读取 MAC/SSID/RSSI，从 chip/flash/partition API 读取平台元数据，板卡类型固定为项目板型标识。
- [x] 增加可选的本机预注册序列号 Kconfig 字段：为空时固件发版本 1；配置有效序列号时发版本 2 与 `serial_number`。对外发送的 MAC 必须与 `Device-Id` 一致。
- [x] 固件在发送日志中记录完整 `device_info` JSON，确保不包含设备到 Go 的 bearer token 或 Wi-Fi 密码；服务端原样记录该 JSON。将 WebSocket 文本读上限设为 16 KiB。二进制 WebSocket 上行与下行各按帧记录方向、时间、设备/会话 ID 和完整 Base64 PCM，不截断，并添加日志断言验证编码后的内容可还原为原始帧。
- [x] 运行 `cd server && go test ./internal/device -count=1`。
- [ ] 在 ESP-IDF 环境运行 `cd firmware/esp32s3 && idf.py build`；当前主机未安装 `idf.py`，固件编译待有 ESP-IDF 的环境验证。
- [x] 提交：`git add server/internal/device/protocol.go server/internal/device/websocket.go server/internal/device/session.go server/internal/device/session_test.go firmware/esp32s3/main/backend_client.c firmware/esp32s3/main/backend_client.h firmware/esp32s3/main/Kconfig.projbuild firmware/esp32s3/main/CMakeLists.txt firmware/esp32s3/README.md && git commit -m "feat: report OTA metadata from ESP32"`。

### Task 3: 本地激活码语音合成

**Files:**
- Create: `server/internal/speech/espeak.go`
- Create: `server/internal/speech/espeak_test.go`
- Modify: `deploy/Dockerfile`

**Interfaces:**
- `speech.Espeak.Synthesize(ctx, activationCode string) ([][]byte, error)` 直接执行 `espeak-ng`，输出设备格式的 640 字节 PCM 帧。
- WAV 转换仅接受 RIFF/WAVE PCM16 mono，解析实际输入采样率后转为 16 kHz，拒绝空数据、畸形 chunk、超限输出和不支持的编码。
- 合成使用 `exec.CommandContext`，不经过 shell；命令时限 10 秒，WAV 上限 2 MiB；日志保留激活码、WAV 和 PCM 帧原始值，音频以完整 Base64 字段记录。

- [x] 编写 WAV 转换测试：16 kHz 输入按 640 字节切帧；22050 Hz 输入重采样后生成完整 20 ms 帧；尾部不完整样本、非 PCM16、非单声道、缺失 data chunk、畸形 RIFF chunk 和超限输入均返回错误。
- [x] 为命令执行添加 fake executable 测试，验证参数使用 `-v cmn --stdout`、输入文本将六位码逐位转成普通话数字（例如 `123456` 转成“灵矽激活码：一，二，三，四，五，六”）、超时/非零退出/超限输出均返回错误且不会写到 shell。
- [x] 运行 `cd server && go test ./internal/speech -count=1`，确认测试先因包和行为缺失而失败。
- [x] 实现有界命令输出、RIFF chunk 解析、16-bit PCM 线性重采样和 640 字节分帧；日志记录原始输入码、WAV 和输出 PCM。
- [x] 在运行镜像的 Debian 软件包安装列表加入 `espeak-ng`，不添加 TTS 模型或 FFmpeg 依赖。
- [x] 重跑 `cd server && go test ./internal/speech -count=1`，确认合成转换测试通过。
- [x] 提交：`git add server/internal/speech deploy/Dockerfile && git commit -m "feat: synthesize Linx activation prompts"`。

### Task 4: 单设备绑定状态机与 Linx 凭证刷新

**Files:**
- Create: `server/internal/device/activation.go`
- Modify: `server/internal/device/session.go`
- Modify: `server/internal/device/linx_session.go`
- Modify: `server/internal/device/linx_session_test.go`
- Modify: `server/internal/linx/client.go`
- Modify: `server/internal/linx/client_test.go`

**Interfaces:**
- `device.OTAProvider` 暴露 Task 1 的 `Report` 和 `CheckActivation`；`device.ActivationSpeaker` 暴露 `Synthesize(ctx, code) ([][]byte, error)`。
- `device.StartOTADeviceSession(session, provider, speaker)` 等待该 session 的元数据，然后运行完整 OTA、播报、轮询和 Linx 生命周期。
- 接口签名固定为 `Report(context.Context, string, string, string, ota.DeviceInfo) (ota.Binding, error)`、`CheckActivation(context.Context, string) (bool, error)`、`Synthesize(context.Context, string) ([][]byte, error)`。
- `Session.DeviceInfo` 由 Task 2 提供；`StartOTADeviceSession(*Session, OTAProvider, ActivationSpeaker)` 仅启动一次并以 `session.Done` 取消所有任务；绑定状态日志原样记录激活码、Linx token 和 OTA 凭证。
- `linx.Client.Run` 在 WebSocket 握手得到 401/403 时返回可通过 `errors.As` 识别的 `linx.AuthError`，并保留 HTTP 状态码。
- 控制泵只为一个 session 启动一次；OTA token 不进入设备消息或持久层；Linx 客户端使用 OTA 返回的 URL/token 和 session 的持久 `Client-Id`。

- [x] 为 Linx 401/403 添加 typed error 测试；分别验证 401/403 可分类、其他状态仍作为普通连接错误。
- [x] 运行 `cd server && go test ./internal/linx -run TestClientSurfacesAuthenticationFailure -count=1`，确认新增分类断言失败。
- [x] 在 Linx dial 错误中保留 HTTP 状态并增加 `AuthError`，不改变成功握手、音频协议和其他错误处理。
- [x] 为 OTA 状态机写 fake provider/speaker/Linx peer 测试：未绑定先播报且不连接 Linx；202 和临时轮询错误继续等待；success 后重新 OTA；刷新暂缺 token 时不建立连接并继续重试；完整 URL/token 才连接；语音合成失败后保留等待并重试；认证失败刷新 OTA；临时 Linx 错误复用当前凭证退避重连；session.Done 取消所有 worker。
- [x] 用可注入的内部 activation timing 覆盖首次播报、60 秒重播、5 秒起始轮询、30 秒封顶退避；生产 timing 固定为 spec 值，测试 timing 仅缩短等待。
- [x] 添加双设备测试，交错注入不同激活码、token 和音频并验证每台设备只收到自己的下行帧；日志断言会包含已确认需要记录的原始值。
- [x] 实现每会话状态机：设备元数据到达后 OTA report；临时错误指数退避 1–30 秒；pending 状态立即播报并按 60 秒重播，同时按 5–30 秒检查绑定（202 和临时网络/平台错误共用此轮询退避）；success 后重新 report；缺 token 时重试；bound 时建立 Linx 会话。
- [x] 绑定等待时语音帧按序阻塞写入现有 20 ms 设备下行，不丢掉队列较长的激活语音；会话取消时停止排队。
- [x] Linx 401/403 立即回到 OTA report 刷新凭证；其他 Linx 断线按 1–30 秒退避使用当前 URL/token 重连。
- [x] 重跑 `cd server && go test ./internal/linx ./internal/device -count=1`，确认协议与状态机测试通过。
- [x] 提交：`git add server/internal/device/activation.go server/internal/device/session.go server/internal/device/linx_session.go server/internal/device/linx_session_test.go server/internal/linx/client.go server/internal/linx/client_test.go && git commit -m "feat: manage per-device Linx OTA binding"`。

### Task 5: 服务接线、部署文档与全套验证

**Files:**
- Modify: `server/cmd/intercom-server/main.go`
- Modify: `deploy/compose.yaml`
- Modify: `deploy/server.env.example`
- Modify: `deploy/smoke-test.sh`
- Modify: `deploy/README.md`
- Modify: `README.md`

**Interfaces:**
- `main` 为每个新 session 启动音频路由和 `StartOTADeviceSession`，不再读取静态 Linx URL/token 映射。
- Compose 和环境模板只保留设备认证、MCP、KWS、Janus 配置；删除 `LINX_WS_URL` 与 `LINX_DEVICE_TOKENS`。
- smoke test 不配置 Linx 静态凭证并保持现有模拟器健康检查、音频、重连和 SIGTERM 检查。

- [x] 更新配置/文档测试检查项：`rg -n 'LINX_WS_URL|LINX_DEVICE_TOKENS' README.md deploy` 应只命中解释已移除配置的迁移文字，运行配置不再定义两项变量。
- [x] 替换 `main.go` 的静态 token 解析/启动分支为 OTA client、espeak speaker 和每 session OTA worker；由 `go test -race ./...` 的 device 状态机测试验证状态机；保留 DEVICE_TOKENS、MCP、KWS、Janus 初始化行为。
- [x] 从 Compose、env example、smoke test 和部署说明删除静态 Linx URL/token 设置，说明 OTA 自动绑定、扬声器激活码流程及日志中会出现的原始 SSID/激活码/token/音频数据；以 16 kHz PCM16 为例注明每方向原始音频约 2.76 GB/日，Base64 日志约 3.68 GB/日；明确仍禁止记录设备 bearer token、MCP token 和 Wi-Fi 密码。
- [x] 运行 `cd server && go test -race ./...`，确认所有包与端到端测试通过。
- [x] 运行 `cd server && go vet ./...`，确认静态检查通过。
- [ ] 运行 `bash deploy/smoke-test.sh`，确认镜像能安装 `espeak-ng`，设备鉴权、音频、重连与优雅退出工作；已尝试运行，但本机 Docker daemon 不可用，无法启动 smoke 项目。
- [x] 若本机已配置 ESP-IDF 5.3.x，运行 `cd firmware/esp32s3 && idf.py build`；若未配置，记录构建未执行并保留真实硬件/账号验证为部署前验收项。
- [x] 运行 `git diff --check` 并检查工作区；隔离 worktree 保持干净，原始 checkout 中用户已有的 `provisioning.c` 改动和 Vim swap 文件已保留且未暂存。
- [x] 提交：`git add server/cmd/intercom-server/main.go deploy/compose.yaml deploy/server.env.example deploy/smoke-test.sh deploy/README.md README.md && git commit -m "feat: enable automatic Linx device binding"`。

## 执行状态

- 本次复核：`go test -race -count=1 ./...`、`go vet ./...`、`bash -n deploy/smoke-test.sh`、带临时验证值的 `docker compose config --quiet` 和 `git diff --check` 均通过。
- OTA 请求/响应、激活语音合成、Linx 握手、绑定状态机和 session 清理日志带设备/会话关联信息及操作耗时；测试验证 OTA 与语音日志字段。
- `bash deploy/smoke-test.sh` 在 `docker info` 阶段退出：本机没有 Docker daemon socket，未创建容器或临时 Compose 项目；Docker 环境可用后仍需执行此项。
- `idf.py` 和宿主 `espeak-ng` 均未安装；ESP-IDF 固件构建及真实普通话合成/硬件播放尚未验证。`espeak-ng` fake executable 测试通过。
- 代码审查发现的远程明文 WebSocket 凭证风险已由 `24e115b` 修复；OTA 和 Linx 连接端都校验 URL，远程地址必须使用 `wss://`。
- 实现、TLS 修复和 ESP32-S3 板级基线均已推送；远端 `origin/main` 当前为 `dba63b5`。

## 完成验收

- 绑定设备无需手动配置 `LINX_WS_URL` 或 `LINX_DEVICE_TOKENS`；OTA bound 响应的 URL/token 只驱动对应设备的虚拟 Linx 会话。
- 未绑定设备会通过实体扬声器播报六位激活码，并依照有界轮询等待用户在灵矽控制台绑定。
- 设备断开或会话过期后，绑定轮询、TTS 子进程和 Linx worker 都能按 session context 停止；重连不产生重复 worker。
- 所有自动化 Go 测试、`go vet` 和可运行的 Compose smoke test 通过；硬件、真实账号或未安装的 ESP-IDF 环境限制在最终结果中明确列出。
- 相关里程碑均已提交，最终实现已推送到 `origin/main`。
