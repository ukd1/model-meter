# Verification — 2026-10-29

- `go test -race ./...`: passed. Covers JSON accounting, passthrough, body/credential exclusion, streaming flush, fragmented SSE, Responses usage, missing usage, upstream error status, revocation, full-queue nonblocking behavior, user isolation, profile updates, and disabled-account enforcement.
- `go vet ./...`: passed.
- `go build -o model-meter .`: passed on linux/arm64.
- Playwright Chromium: passed administrator/user sign-in, user creation, key creation, model request with a user key, own profile edit, two live SVG graphs, mobile page width at 390 px, key revocation, and user disable. Inspected desktop screenshot. Screenshots: `/tmp/model-meter-desktop.png` and `/tmp/model-meter-mobile.png`.
- Actual upstream: `http://127.0.0.1:8000`, model `deepseek-v4.1-flash`. Proxied `/v1/models` returned HTTP 200. Non-streamed chat returned HTTP 200 with 35 input / 15 output tokens, recorded duration 60,670 ms. Streamed chat returned HTTP 200 with 35 input / 11 output tokens, recorded duration 53,913 ms. Final usage matched persisted metadata. Upstream was concurrently processing a large-context request; these durations are not proxy overhead measurements.
- Test client keys revoked and browser QA user disabled after verification; their metadata remains visible as sample history.
- User systemd service enabled and active on port 8001. Existing user linger is enabled. Restart verified: four request records persisted, health endpoint returned OK, zero dropped accounting records, test key revocations and disabled user state survived.

No inference bodies or credentials are included in this report or committed to Git.

## DS4 protocol compatibility update — 2026-10-29

Inspected DS4's HTTP route dispatch and usage serializers. Added Anthropic Messages and individual model lookup; verified the existing OpenAI Responses, Chat Completions, and legacy Completions paths.

- `go test -race ./...`, `go vet ./...`, and build passed after implementation.
- Added tests for Anthropic JSON/SSE passthrough, partial and cumulative usage snapshots, cache-read/cache-write normalization, Responses JSON/SSE envelopes, credential replacement, protocol headers, private-header exclusion, conflicting credentials, revoked `x-api-key` authentication, and model-route log normalization.
- `python3 tests/protocol_smoke.py` passed against the actual DS4 upstream through the deployed service. Model list and individual model lookup both succeeded.

| Endpoint | JSON input/output | Stream input/output | JSON / stream duration |
| --- | --- | --- | --- |
| `/v1/messages` | 33 / 4 | 33 / 4 | 1,280 / 1,111 ms |
| `/v1/responses` | 33 / 4 | 33 / 4 | 1,107 / 1,168 ms |
| `/v1/chat/completions` | 33 / 4 | 33 / 4 | 1,211 / 1,199 ms |
| `/v1/completions` | 36 / 4 | 36 / 4 | 1,432 / 1,273 ms |

All eight inference records matched the upstream's usage after normalization. These durations include inference and are not proxy-overhead measurements. Test credentials were revoked; request metadata remains as verification history. The service is active on port 8001. This verifies Anthropic protocol compatibility with DS4, not a deployment against Anthropic's hosted service.
