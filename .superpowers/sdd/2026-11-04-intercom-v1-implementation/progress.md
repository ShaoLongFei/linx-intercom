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
