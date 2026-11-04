# Project Instructions

## Product Direction

- This repository is the home for all intercom firmware, backend code, deployment files, and project documentation.
- Target device: ESP32-S3. Firmware uses ESP-IDF with C/C++.
- Backend: Go. Janus Gateway with AudioBridge runs as a separate server-side media service.
- Device-side wake-word detection must keep the wake phrase out of room audio. Buffer and route microphone audio locally before forwarding it.

## Working Preferences

- Commit each meaningful, reviewable milestone promptly.
- Keep tests minimal; the user handles most testing. Add useful structured logs around state changes and failure paths to support debugging.
- Prefer small, direct implementations. Do not add abstractions without a second caller or a demonstrated need.
- Keep personal device settings, credentials, and local environment details out of tracked files.
- Add Chinese comments only where they clarify non-obvious behavior.
