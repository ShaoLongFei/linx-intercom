# SDD ledger — plan: docs/superpowers/plans/2026-11-04-intercom-v1-implementation.md

## Setup

- Spec: `docs/superpowers/specs/2026-11-04-intercom-v1-design.md` is present and authoritative.
- Plan BASE: `b8c763c` on `main`; direct main work is authorized by the user's request to push the implementation to the main line.
- Pre-flight shared interfaces: Task 1 produces device sessions consumed by Tasks 2, 3, 4, and 5. The plan's framing was too generic, so the ruling below fixes the initial wire contract before code.
- Ruling: authenticate each device WebSocket with `Device-Id` plus a per-device Bearer token; keep a server-side verifier interface and never reuse Linx credentials — required because the spec requires device authentication but leaves enrollment details open; cost if wrong is a later token provisioning migration.
- Ruling: use JSON text frames for control and binary PCM16 mono 16 kHz frames for audio in v1 device-to-server transport — simple to test with Linux WAV input and consistent with Linx's documented PCM input; cost is higher bandwidth than Opus and server transcoding for Janus.
- Ruling: use `github.com/coder/websocket` for context-aware WebSocket server/client support — standard library has no WebSocket implementation; cost is one focused dependency.

## Tasks

- Task 1: in progress. BASE `b8c763c`.
Task 1: complete (commits b8c763c..b8c763c, tests: go test ./... → ok  	linx-intercom/server/internal/device	0.028s)
Task 2: complete (commits 7c87c10..7c87c10, tests: sh -c 'cd server && go test ./... && go vet ./... && cd .. && git diff --check' → ok  	linx-intercom/server/internal/device	0.028s)
- Task 3: complete (commit 9e7b3d5; race tests, vet, and local health/MCP authorization smoke checks passed).
- Task 4: complete (Pion audio RTP and fake Janus WebSocket/WebRTC tests, race suite, vet, Compose config check passed).
- Task 4: Ruling: use PCMU/8 kHz for Janus and pure-Go companding/resampling — device transport is PCM16 16 kHz and no portable pure-Go Opus encoder was available; this keeps deployment CGO-free at the cost of narrowband voice quality.
- Task 4: External check: Janus image pull/real container smoke test not run because registry network access failed in this environment; retry where the image can be pulled.
- Task 4 follow-up: SQLite now persists unique Janus room IDs; existing databases migrate on open. MCP create-room and accept-invitation call the online device media join hook, rollback store changes on join errors, and session expiry releases its media peer. Dynamic create/exists/destroy uses the AudioBridge API and has fake signaling coverage. A regression test confirms HTTP request cancellation does not close a live Janus signaling session.
- Task 4 follow-up verification: `go test -race ./...`, `go vet ./...`, `git diff --check`, and Janus compose config validation passed.
- Task 7 progress: added a Go/Janus Compose deployment, CGO-free Go container, persistent SQLite volume, safe environment template, and `/healthz` container probe. Compose config, `go test -race ./...`, `go vet ./...`, and diff check passed. Container image build and service startup remain unverified because registry image pull previously failed.
- Task 5: pending wake phrase/model choice is still an external product input; implement routing state machine with a deterministic detector seam, but do not claim wake-word detection itself is complete without a supported model and samples.
- Task 5 follow-up: added configurable sherpa-onnx streaming KWS for Linux amd64/arm64 deployments, per-session detector cleanup, persistent UUID v4 Linx Client-Id, model configuration validation, and KWS deployment notes. Wake-enabled sessions use a 50-frame lookback; disabled sessions use a one-frame lookback to avoid adding an unnecessary one-second delay.
- Task 5 follow-up verification: `go test -race ./...`, `go vet ./...`, `docker compose --env-file deploy/server.env.example -f deploy/compose.yaml config --quiet`, and `git diff --check` passed. Docker image build was not possible because the local Docker daemon is unavailable. No wake phrase, model files, or labeled audio samples were provided, so detection quality and per-device model resource use are not evaluated.
Task 3: complete (commits 6cf8554..6cf8554, tests: sh -c 'cd server && go test ./... && go vet ./... && cd .. && git diff --check' → ok  	linx-intercom/server/internal/store	(cached))
Task 4: complete (commits 9e7b3d5..9e7b3d5, tests: sh -c 'cd server && go test ./... && go vet ./... && cd .. && docker compose -f deploy/janus-compose.yaml config >/dev/null && git diff --check' → ok  	linx-intercom/server/internal/store	(cached))
