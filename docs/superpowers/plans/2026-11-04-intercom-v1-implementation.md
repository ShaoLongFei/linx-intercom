# 灵矽对讲机 v1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver a testable first version of the ESP32 intercom with per-device voice sessions, Linx semantic control, Janus rooms, BluFi provisioning, and voice-driven friend/room operations.

**Architecture:** ESP32 exchanges audio and control events with Go over one authenticated persistent WebSocket. Go owns the per-device session, server-side wake-word routing, Linx WebSocket client, business APIs/MCP tools, and a Pion WebRTC endpoint connected to Janus AudioBridge. Room audio and Linx TTS are selected as the single device downlink source according to session mode.

**Tech Stack:** Go 1.26, `net/http`, SQLite, Pion WebRTC, ESP-IDF for ESP32-S3, ESP-IDF BluFi, Linx WebSocket/MCP protocols, Janus AudioBridge.

**Spec:** `docs/superpowers/specs/2026-11-04-intercom-v1-design.md`

## Global Constraints

- One ESP32 maps to one stable user identity and one isolated virtual Linx identity/session.
- The initial disconnect grace period is 60 seconds; reconnect during the grace period resumes the device session, and stale audio is discarded.
- Linx control mode pauses Janus microphone publishing and room downlink playback; Linx TTS becomes the device downlink until control completes.
- Invitations are accepted only for online devices; offline invitations fail immediately.
- BluFi is the only v1 provisioning path; validate phone-side support for custom devices before promising a ready-made app flow.
- Emit structured logs for lifecycle transitions and failures. Never log audio samples, Wi-Fi passwords, tokens, or credentials. Explain non-obvious business rules in Chinese comments.
- Keep server and firmware secrets in environment/local configuration, never tracked files.

## Review Focus

- Reconnect races or duplicate device connections must not create two active sessions for one device; test concurrent reconnect and timeout cleanup.
- Audio arriving around a wake-word transition must not leak room speech into Linx or stale room audio into control playback; test mode-switch buffer boundaries.
- An unbound or invalid device must not create a session; test authentication before allocating media resources.
- A Linx/Janus failure must release or retain resources according to the 60-second grace rule; test both transient reconnect and expiration.
- BLE provisioning must not leave credentials in logs or keep the provisioning service open after success; test success, wrong password, and reset-to-provisioning.

---

### Task 1: Device WebSocket and session lifecycle

**Files:**
- Create: `server/internal/device/protocol.go`
- Create: `server/internal/device/session.go`
- Create: `server/internal/device/websocket.go`
- Create: `server/internal/device/session_test.go`
- Modify: `server/cmd/intercom-server/main.go`
- Modify: `server/go.mod`

**Interfaces:**
- `SessionManager.Connect(ctx context.Context, deviceID string, conn *websocket.Conn) (*Session, error)` authenticates and returns the single active session for that device.
- `SessionManager.Disconnect(deviceID string)` starts a 60-second reconnect grace timer.
- A session carries one device ID, one generated user ID, mode, cancellation context, and audio send channel. Audio channels are bounded; overflow drops the oldest stale frame and logs the drop.

- [ ] Add tests for unauthenticated upgrade rejection, one session per device, reconnect replacing the old socket, grace-period retention, expiry cleanup, and stale audio discard.
- [ ] Run `cd server && go test ./internal/device` and confirm the lifecycle tests fail before implementation.
- [ ] Implement authenticated WebSocket upgrade, bounded binary audio frames, JSON control events, reconnect-safe session replacement, and timer cancellation.
- [ ] Add structured lifecycle/error logs with device/session correlation IDs and Chinese comments for ownership and timer race handling.
- [ ] Run `cd server && go test ./...` and `git diff --check`.

### Task 2: Linux virtual device simulator

**Files:**
- Create: `server/cmd/intercom-sim/main.go`
- Create: `server/internal/device/testdata/README.md`
- Create: `server/internal/device/simulator_test.go`
- Modify: `README.md`

**Interfaces:**
- Simulator flags: `-server`, `-device-id`, `-token`, `-audio-file`, `-reconnect-after`; it uses the same device WebSocket framing as ESP32.
- The simulator can send PCM audio from a WAV file, receive/playback-save downlink audio, and deliberately disconnect/reconnect.

- [ ] Test audio frame and control-event framing with an in-process server and temporary WAV fixture.
- [ ] Implement the CLI simulator using the standard library WAV reader for PCM16 mono and the production WebSocket client package.
- [ ] Run a local server plus simulator and verify bidirectional frames, reconnect within 60 seconds, and cleanup after expiry.
- [ ] Document exact local commands and expected structured log fields in `README.md`.

### Task 3: Linx virtual-device client and MCP business tools

**Files:**
- Create: `server/internal/linx/client.go`
- Create: `server/internal/linx/client_test.go`
- Create: `server/internal/store/store.go`
- Create: `server/internal/store/store_test.go`
- Create: `server/internal/mcp/tools.go`
- Create: `server/internal/mcp/tools_test.go`
- Modify: `server/cmd/intercom-server/main.go`
- Modify: `server/go.mod`
- Create: `server/internal/device/linx_session.go`

**Interfaces:**
- `linx.Client.Run(ctx, deviceID, audioIn, audioOut, eventsIn, eventsOut)` owns one Linx WebSocket per physical device and handles hello/listen/audio/TTS frames.
- SQLite persists device-to-user identity, friendships, one-time pairing codes, rooms, and membership; online status remains runtime session state.
- MCP tools: `create_room`, `pair_friend`, `accept_friend`, `invite_member`, and `list_online_members`. Tool calls resolve identity from Linx request metadata and cannot choose another device's identity from model arguments.

- [ ] Test Linx hello/auth failure, PCM/Opus frame handling per negotiated format, TTS downlink, and session cancellation with an `httptest` WebSocket peer.
- [ ] Test persistence, one-time/expiry behavior for pair codes, both-party friend confirmation, online-only invitations, and online-member filtering.
- [ ] Implement Linx client using OTA-provided URL/token configuration; never log tokens or audio payloads.
- [ ] Implement SQLite-backed domain operations and MCP JSON-RPC handlers with validation and structured failure logs.
- [ ] Start one Linx client for each device session, retry transient failures with backoff, and cancel it when the session expires.
- [ ] Run `cd server && go test ./...` and `go vet ./...`.

### Task 4: Janus AudioBridge media endpoint

**Files:**
- Create: `server/internal/media/janus.go`
- Create: `server/internal/media/audio.go`
- Create: `server/internal/media/janus_test.go`
- Create: `deploy/janus-compose.yaml`
- Modify: `server/go.mod`
- Modify: `deploy/README.md`

**Interfaces:**
- `MediaSession.Publish(ctx, deviceAudioIn)` publishes device microphone frames to one Janus AudioBridge participant.
- `MediaSession.Subscribe(ctx) -> deviceAudioOut` emits the room mix for that participant.
- Each device media session joins one configured room and releases Pion/Janus resources on session expiry.

- [ ] Add protocol tests with a fake Janus signaling peer for join, publish, subscribe, disconnect, and cleanup failures.
- [ ] Implement Pion WebRTC negotiation and Janus AudioBridge signaling; keep credentials and room secrets in environment variables.
- [ ] Implement single-source downlink selection so Janus mix is suppressed whenever Linx control/TTS owns playback.
- [ ] Run media tests and a local Janus compose smoke test with two Linux simulators; verify each hears the other's audio and neither receives its own mix.
- [ ] Document firewall ports, TURN/STUN configuration points, health checks, and teardown behavior.

### Task 5: Wake-word detection and routing state machine

**Files:**
- Create: `server/internal/audio/router.go`
- Create: `server/internal/audio/router_test.go`
- Create: `server/internal/audio/wakeword.go`
- Create: `server/internal/audio/testdata/README.md`
- Modify: `server/internal/device/session.go`

**Interfaces:**
- `WakeWordDetector.Process(frame) (detected bool, err error)` consumes the configured PCM format and is replaceable behind this small interface.
- Router states are `room` and `control`; transition to control stops Janus publish/downlink, notifies Linx listen-detect, then forwards post-wake command audio. Linx completion returns to room and clears stale buffers.

- [ ] Test transitions with a deterministic fake detector, asserting pre-wake audio goes only to Janus and post-wake audio goes only to Linx.
- [ ] Test detector errors, frame gaps, repeated wake triggers, Linx failure, TTS stop/abort, and stale downlink clearing.
- [ ] Evaluate an on-device-independent server wake-word engine on Linux using recorded wake/no-wake samples; select a maintained engine/model that can run in the Go service deployment environment and record CPU/RAM and false-trigger results.
- [ ] Implement the selected engine adapter without committing licensed/user audio samples or model credentials; add Chinese comments for transition ordering.
- [ ] Run unit tests and replay the sample corpus through the simulator; confirm no test audio crosses both routes.

### Task 6: ESP32-S3 BluFi provisioning and audio client

**Files:**
- Create: `firmware/esp32s3/main/provisioning.c`
- Create: `firmware/esp32s3/main/audio.c`
- Create: `firmware/esp32s3/main/backend_client.c`
- Create: `firmware/esp32s3/main/device_config.c`
- Modify: `firmware/esp32s3/main/main.c`
- Modify: `firmware/esp32s3/main/CMakeLists.txt`
- Modify: `firmware/esp32s3/CMakeLists.txt`
- Create: `firmware/esp32s3/README.md`

**Interfaces:**
- Provisioning uses ESP-IDF BluFi, persists Wi-Fi configuration in NVS, and exposes an explicit reset-to-provisioning action.
- Audio client sends the negotiated frame format and control events to the authenticated Go WebSocket, and plays selected downlink audio through I2S/Codec.
- Hardware pins and codec model are configuration values documented for the selected test board, not committed personal settings.

- [ ] Add component-level tests or host-buildable checks for frame serialization, mode events, and credential redaction where ESP-IDF permits.
- [ ] Implement BLE provisioning, Wi-Fi scan results, credential storage, success/failure status, and close BLE provisioning after success.
- [ ] Implement I2S capture/playback, WebSocket reconnect with the server grace period, audio queue bounds, and structured ESP logs.
- [ ] Build with `idf.py set-target esp32s3 build`; document board-specific pin/codec settings and required Linx BluFi app compatibility.
- [ ] Validate with the Linux simulator protocol fixtures and then record actual board, PSRAM, free heap, and audio format measurements when hardware is available.

### Task 7: Local deployment and operational runbook

**Files:**
- Create: `deploy/Dockerfile`
- Create: `deploy/compose.yaml`
- Create: `deploy/server.env.example`
- Modify: `deploy/README.md`
- Modify: `README.md`

**Interfaces:**
- `docker compose -f deploy/compose.yaml up` starts Go service and Janus with persistent SQLite storage and health checks.
- Required secrets are injected through environment variables; the example file contains names and safe placeholders only.

- [ ] Add a deployment smoke test that starts services, checks `/healthz`, opens a simulator session, and verifies clean shutdown.
- [ ] Implement minimal container definitions, persistent data volume, health checks, and network ports needed for HTTP/WebSocket and Janus WebRTC.
- [ ] Document DNS/TLS reverse-proxy expectations, UDP media ports, secret setup, database backup, logs, and upgrade/restart behavior.
- [ ] Run the smoke test and `git diff --check`; confirm no local credentials or device settings are tracked.

### Task 8: End-to-end voice workflow verification

**Files:**
- Create: `server/test/e2e/intercom_flow_test.go`
- Modify: `README.md`
- Modify: `docs/architecture.md`

**Interfaces:**
- Two simulator clients exercise identity isolation, room creation, online invite/accept, member query, control-mode speech, Linx TTS, reconnect, and timeout cleanup through public service interfaces.

- [ ] Test end-to-end domain workflow against a local SQLite database and fake Linx/Janus peers.
- [ ] Run the two-client local compose scenario and verify room audio downlink and exclusive Linx TTS playback.
- [ ] Update architecture docs to reflect server-side wake detection and BluFi provisioning; remove stale device-local wake-word routing statements.
- [ ] Run `cd server && go test ./... && go vet ./...`, firmware build if ESP-IDF is available, and `git diff --check`.
- [ ] Record integration gaps that require the user's Linx account, phone app, physical board, or production server without claiming those external checks passed.
