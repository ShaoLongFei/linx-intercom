# ESP32-S3 firmware

This firmware uses ESP-IDF 5.3.x, BluFi for first-time Wi-Fi setup, 16 kHz mono PCM16 over the authenticated device WebSocket, and full-duplex standard I2S for capture/playback. Device wake-word detection stays on the Go server so the wake phrase is removed before room audio is routed.

## Build and configure

```sh
cd firmware/esp32s3
idf.py set-target esp32s3
idf.py menuconfig
idf.py build
idf.py -p PORT flash monitor
```

In `Linx Intercom`, set the per-device bearer token and Go WebSocket URI. The token lives in the local ignored `sdkconfig`; do not copy it into tracked files or logs. `Device-Id` is derived from the ESP32-S3 station MAC in uppercase colon-separated form, matching the server's registered device ID.

`INTERCOM_SERIAL_NUMBER` is optional. Leave it blank for OTA activation version 1. Set it only when the device has a valid pre-registered serial number; the firmware then reports activation version 2 and sends the serial number in `device_info`.

Configure BCLK, WS, DIN, DOUT, optional MCLK, amplifier enable GPIO/polarity, and reset-button GPIOs for the actual board. Defaults are placeholders only. The current audio component configures digital I2S pins; the exact codec's I2C register initialization, analog gain, mute, and board-specific clock requirements must be added after the codec is selected. Enable PSRAM in `menuconfig` when the chosen module provides it.

Set a reset-button GPIO and hold its active-low button for three seconds to erase saved Wi-Fi configuration and reboot into BluFi provisioning. With the reset GPIO disabled, call `provisioning_reset_wifi()` from the board's chosen hardware action.

## Provisioning and transport

The device advertises as the ESP-IDF BluFi profile name, scans nearby Wi-Fi networks on request, and returns the network list to the BluFi client. The phone selects an SSID and submits the password through the BluFi encrypted channel. Credentials are stored by ESP-IDF Wi-Fi NVS. Password values, audio payloads, and backend tokens are never included in firmware logs. Once an IP address is acquired, advertising stops and any active BluFi phone connection is closed.

The device WebSocket handshake sends `Device-Id` and `Authorization: Bearer ...`; after each connection the firmware reports OTA metadata as a `device_info` JSON message. It includes the application ELF SHA-256, board, current Wi-Fi SSID/RSSI, STA MAC, chip, flash and partition data. The Go backend uses one binary message per 20 ms PCM16 mono 16 kHz frame and JSON text messages for control. After normal `tts.stop`, firmware queues a playback-drained marker behind audio, writes a silence frame, waits 100 ms for the configured I2S DMA ring to finish, then sends `{"type":"playback","state":"drained"}`. The Go server restores room routing only after this acknowledgement. Aborted TTS clears the device queue before the room-mode event. This endpoint is the intercom backend protocol, so the firmware does not send Linx's separate `hello` handshake; the Go server owns the isolated Linx session. WSS uses ESP-TLS's CA bundle. Audio frames are sent only while connected; disconnected microphone frames are discarded to prevent stale speech after reconnection.

Use the Linx app only if it supports scanning and configuring a custom ESP-IDF BluFi device. Otherwise use Espressif's BluFi client (`EspBlufi` on Android or `EspBlufiForiOS`) for the first device test. Compatibility with the Linx app has not been confirmed.

## Device checks

After flashing, confirm logs show Wi-Fi/BluFi state transitions without credential or audio contents, then verify the device appears online in the Go service. Test a wrong password, a successful 2.4 GHz connection, the three-second reset action, backend interruption shorter and longer than 60 seconds, and bidirectional playback. Record the board, codec, PSRAM size/free heap, measured sample rate, and end-to-end latency during these checks.
