# Linx OTA Automatic Binding Design

## Goal

Remove manual Linx WebSocket URL/token provisioning from the server. For each physical ESP32-S3, the Go service will request OTA configuration using that device's identity, report the activation code through the device speaker, and automatically detect when the user has bound the device in the Linx console. After binding, Go will request OTA configuration again and start that device's isolated Linx WebSocket session.

The only user action during first binding is entering the six-digit activation code in the Linx console and selecting the target agent. No Linx account API credential is stored by this project.

## Non-goals

- Automatically signing in to a Linx user account or calling the authenticated device-bind API.
- Changing device-to-Go authentication (`DEVICE_TOKENS`) or MCP authentication (`MCP_API_TOKEN`).
- Sending the Linx WebSocket token to ESP32.
- Changing the room, friendship, media, or wake-word workflows.
- Adding a general-purpose local TTS subsystem. Local speech is used only for the short activation prompt.

## User Flow

1. ESP32 joins Wi-Fi and connects to the authenticated Go device WebSocket.
2. ESP32 sends one `device_info` control message containing the OTA-required firmware, board, and current Wi-Fi metadata.
3. Go submits an OTA report for that device.
4. If OTA returns an activation code but no WebSocket token, Go synthesizes a Chinese prompt with local `espeak-ng` and streams it to the ESP32 speaker using the existing PCM downlink. It repeats the code on first receipt and every 60 seconds while the device remains unbound.
5. The user enters the six-digit code in the Linx console and binds the device to the desired agent.
6. Go polls the OTA activation check. When Linx reports the device bound, Go submits OTA again, receives the WebSocket URL/token, and starts the per-device Linx session.
7. The activation prompt stops. The Linx token remains only in the in-memory device session.

The OTA activation code is not submitted back to Go. The user enters it directly in the Linx console.

## Components and Data Flow

### Device metadata

The physical station MAC remains the device identity. It is sent in the existing `Device-Id` WebSocket header and must match OTA's `mac_address` field. The existing persistent UUID v4 Linx `Client-Id` remains stable across ESP32 reconnects and server restarts.

After the device WebSocket is attached, firmware sends a JSON text message of type `device_info` with the fields needed to form the OTA request:

- Application name/version, ESP-IDF version, compile time, and firmware ELF SHA-256.
- Chip model, board type, flash size, and any supported partition metadata.
- Current Wi-Fi SSID and RSSI; SSID is required by the documented OTA board schema.
- Station MAC, which must equal `Device-Id`.
- `Activation-Version`, derived from the device's serial-number/eFuse provisioning state. Current v1 devices without a pre-registered serial use version `1`; devices with a valid provisioned serial use version `2` and include that value as optional `serial_number` metadata.

Go validates the message shape and that the reported MAC matches the authenticated WebSocket identity. It does not log SSID, firmware hashes, activation codes, or tokens. Device metadata is runtime session data and is not persisted in SQLite.

### OTA registration

Go posts to `https://xrobo.qiniuapi.com/v1/ota/` using the `Activation-Version` reported by ESP32, `Device-Id`, persistent `Client-Id`, `User-Agent`, and the firmware/board JSON reported by ESP32. For activation version `2`, Go also sends the reported serial as the `Serial-Number` request header; version `1` omits it. `mac_address` must match `Device-Id`; `board.type`, `board.ssid`, `board.rssi`, `application.version`, and `application.elf_sha256` must be populated from the actual board/runtime/build metadata rather than placeholder values. Go rejects inconsistent combinations of activation version and serial metadata before making the OTA request.

The response is interpreted as follows:

- `websocket.token` and `websocket.url` present: the device is bound; start or refresh its Linx WebSocket session.
- `activation.code` present and no token: the device is waiting for the user to bind it; synthesize and play the code, then begin activation polling.
- Neither a token nor an activation code: treat the response as a protocol/registration error and log the sanitized stage and response status, not the response body.
- OTA business errors must be checked in the JSON body even when the HTTP status is 200.

The OTA activation check is `POST https://xrobo.qiniuapi.com/v1/ota/` at initial registration, then `POST https://xrobo.qiniuapi.com/v1/ota/activate` with `Device-Id` while waiting. A `202` response means still unbound. A successful `200 success` response triggers a fresh OTA report to obtain the URL and token.

### Local activation speech

The server invokes `espeak-ng` directly without a shell, in Mandarin (`cmn` voice where available), to speak the activation code as separated Chinese digits. `espeak-ng` produces WAV audio; Go validates the WAV format, converts it to 16 kHz mono PCM16, splits it into the existing 640-byte/20 ms device frames, and enqueues those frames on the device downlink. Linx TTS and room audio are not prerequisites for activation playback.

The deployment image installs the `espeak-ng` runtime package. No additional TTS model files are required. Speech command duration and output size are bounded. The activation code is passed only to the child process and is never logged. Synthesis failure leaves the authenticated device session alive and retries the prompt on the next 60-second interval.

## State and Credential Lifecycle

- Persistent Linx `Client-Id` remains in SQLite as today.
- OTA metadata, activation code, and returned WebSocket token are held in memory for the active device session only.
- The Linx token is not written to SQLite, logs, or ESP32 storage.
- A process restart closes sessions. When ESP32 reconnects, Go reports OTA again and obtains current binding state/configuration. Linx's documented activation-code cache may return the same pending code; the implementation must not depend on a particular cache duration.
- A Linx WebSocket 401/403 triggers a fresh OTA report before retrying the connection. Other transient Linx errors use the existing reconnect backoff.
- Each physical device keeps its own OTA state, activation polling, TTS output, and Linx session; IDs, audio, or credentials must never cross device sessions.

## Retry and Failure Handling

- Device WebSocket authentication and the 60-second reconnect grace period remain unchanged.
- OTA network errors, HTTP 5xx, and temporary platform errors retry with bounded exponential backoff from 1 second to 30 seconds. They do not disconnect ESP32 from Go or allocate a second session.
- Activation polling begins at 5 seconds and backs off to at most 30 seconds while the platform reports 202. Polling stops on bind success, device session expiry, or cancellation.
- While waiting for binding, Go speaks the code on first receipt and every 60 seconds until binding succeeds or the session expires.
- Invalid OTA payload/business errors are logged with phase and HTTP status and retried at the bounded backoff; response bodies and secrets are not logged.
- Missing/invalid device metadata prevents OTA registration for that session. The device remains connected to Go and firmware may resend metadata after a corrected/reconnected state.
- If the OTA activation check says `success` but the next OTA report still omits a token, Go treats this as a transient protocol state and retries OTA; it must not start Linx with an empty credential.
- `espeak-ng` missing, timeout, nonzero exit, invalid WAV, or PCM conversion error is logged without disconnecting ESP32. The activation attempt remains pending and speech retries after 60 seconds.
- Linx authentication failures refresh OTA configuration; non-authentication errors reconnect with backoff while retaining the device session.

## Configuration Changes

- Remove `LINX_DEVICE_TOKENS` and static `LINX_WS_URL` as required configuration. Linx URL/token come from OTA per device.
- Add the `espeak-ng` OS package to the server container image.
- Preserve `DEVICE_TOKENS`, `MCP_API_TOKEN`, KWS configuration, and optional Janus configuration without behavioral change.
- Keep the Linx OTA host fixed to the documented HTTPS endpoint in code for v1; do not add a public environment override without a demonstrated need.

## Logging and Privacy

Log OTA request start, activation-pending, bind-success, OTA refresh, Linx connection start/result, speech playback result, retries, and session cleanup with device/session IDs and elapsed time. Never log Wi-Fi SSID, activation code, token, complete OTA response, audio samples, or synthesized audio.

## Verification

Automated tests use fake OTA HTTP and Linx WebSocket peers to cover:

- OTA metadata and identity matching; rejection when MAC and authenticated Device-Id differ.
- Unbound response produces only activation speech and no Linx connection.
- `202` continues polling; bind success triggers a second OTA request and then a Linx connection using the returned URL/token.
- OTA HTTP errors and HTTP-200 business errors retry without ending the device session.
- Linx 401/403 causes OTA refresh; transient Linx errors use reconnect backoff.
- Per-device activation/polling/token state isolation and cancellation on session expiry.
- TTS WAV conversion into correctly sized PCM frames; no activation code or token appears in operational logs.
- Firmware emits required `device_info` fields without logging SSID or adding credentials to the payload.

Real-account verification must confirm the OTA payload is accepted for this custom ESP32-S3 board, the six-digit code binds the device in the Linx console, `/ota/activate` transitions to success, and the refreshed WebSocket credentials establish a virtual-device session. Hardware verification must confirm the spoken digits are intelligible through the selected codec/speaker.

## Open Platform Validation

The platform documentation describes OTA request fields and binding responses but does not guarantee custom board acceptance, activation-code lifetime, polling limits, or Linx token lifetime/rotation. The implementation must treat these as runtime outcomes, use bounded retries, and record sanitized diagnostics for the real-account test.

Reference protocols:

- [Linx OTA](https://linx.qiniu.com/docs/xrobot/platform/OTA)
- [Linx WebSocket](https://linx.qiniu.com/docs/xrobot/platform/websocket)
