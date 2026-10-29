# Verification — 2026-10-29

- `go test -race ./...`: passed. Covers JSON accounting, passthrough, body/credential exclusion, streaming flush, fragmented SSE, Responses usage, missing usage, upstream error status, revocation, full-queue nonblocking behavior, user isolation, profile updates, and disabled-account enforcement.
- `go vet ./...`: passed.
- `go build -o model-meter .`: passed on linux/arm64.
- Playwright Chromium: passed administrator/user sign-in, user creation, key creation, model request with a user key, own profile edit, two live SVG graphs, mobile page width at 390 px, key revocation, and user disable. Inspected desktop screenshot. Screenshots: `/tmp/model-meter-desktop.png` and `/tmp/model-meter-mobile.png`.
- Actual upstream: `http://127.0.0.1:8000`, model `deepseek-v4.1-flash`. Proxied `/v1/models` returned HTTP 200. Non-streamed chat returned HTTP 200 with 35 input / 15 output tokens, recorded duration 60,670 ms. Streamed chat returned HTTP 200 with 35 input / 11 output tokens, recorded duration 53,913 ms. Final usage matched persisted metadata. Upstream was concurrently processing a large-context request; these durations are not proxy overhead measurements.
- Test client keys revoked and browser QA user disabled after verification; their metadata remains visible as sample history.
- User systemd service enabled and active on port 8001. Existing user linger is enabled. Restart verified: four request records persisted, health endpoint returned OK, zero dropped accounting records, test key revocations and disabled user state survived.

No inference bodies or credentials are included in this report or committed to Git.
