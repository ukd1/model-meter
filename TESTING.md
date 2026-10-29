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

## Live dashboard redesign — 2026-10-29

- Replaced browser interval polling with an authenticated SSE change stream and event-triggered snapshots. Added a minimal green/violet terminal-style interface with Overview, Requests, and Access views; split embedded HTML/CSS/JavaScript into separate source files.
- `go test -race ./...`, `go vet ./...`, build, and JavaScript syntax checks passed. New tests verify notification delivery after database commit, per-user notification scope, invalid authentication, live token-reset revocation, graceful stream closure, and nonblocking/coalesced slow-subscriber queues.
- Playwright passed administrator/user sign-in, key/user creation, profile editing, graphs, mobile sizing, pushed request counters, external key renames, key revocation, and immediate active-session termination when the user is disabled.
- The reconnect test took the browser offline, observed RECONNECTING, restored connectivity, and verified a new stream reached LIVE. An idle 16-second window spanning a keepalive produced zero dashboard data reads.
- Isolated preview tests observed visible request updates in 119–183 ms. A browser check on the deployed port 8001 observed an update in 250 ms with zero dropped accounting records. These are observed dashboard-update timings, not inference benchmarks or latency guarantees.
- Desktop/mobile screenshots inspected. Synthetic chart data was used only in the isolated preview database, not production. Deployed screenshot: `/tmp/model-meter-live-desktop.png`. Temporary test keys revoked and test accounts disabled.

## Windowed performance charts and immediate updates — 2026-10-29

- Removed the logger's 250 ms batch timer and the browser's 100 ms debounce. The asynchronous writer now drains only already-queued records (up to 100 per transaction), commits immediately, and sends the change event. Browser refreshes are coalesced in a microtask, with no fixed delay.
- Added full-window latency and end-to-end output-speed histograms with nearest-rank p50/p95, plus an output-speed time graph. All graphs now follow the selected window with adaptive bucket sizes. The speed definition is output tokens divided by full request duration, not decode-only speed.
- `go test -race ./...`, `go vet ./...`, build, and JavaScript syntax validation passed. Tests cover more than 200 samples, older-than-one-hour data, interval boundaries, missing/zero-duration usage, excluded non-inference/errors/interrupted requests, all-time windows, empty results, invalid timestamps, and cross-user filtering.
- Isolated browser fixture: 1h/24h/7d selections produced 250/251/252 histogram samples with the expected 100 tok/s median. All five charts rendered, with desktop/mobile screenshots inspected and no horizontal page overflow. Synthetic fixture data remained in the isolated preview database only.
- The full browser workflow passed live updates (18 ms observed), zero idle polling, offline/reconnect, key rename/revocation, and disabled-session enforcement. After deployment, a browser smoke check observed a 16 ms update with zero dropped records; these are observations, not latency guarantees.
- Live performance endpoint verified: histogram counts and time-series rate sample counts reconcile. Service healthy on port 8001; temporary verification key revoked.


## Named accounts, persistent sign-in, and shorter windows — 2026-10-29

- Replaced master/user sign-in tokens with named username/password accounts and administrator/user roles. Passwords use salted PBKDF2-SHA256; initial bootstrap credentials print once to the service console. Local password recovery prints a generated replacement. Existing API keys and usage records remain intact; legacy users retain their IDs and get `user-ID` usernames pending password setup.
- Added 30-day opaque HttpOnly/SameSite cookies, server-side logout, CSRF checks, and session invalidation after disabling an account or changing its password. Cookie sessions do not authorize proxy requests. Password-manager fields now identify username, current password, and new password explicitly. The 1Password extension itself was not available for testing.
- Added windows for 1, 5, and 15 minutes and 1, 4, and 12 hours; retained the longer existing windows.
- Full `go test -race ./...`, `go vet ./...`, build, JavaScript syntax, and diff checks passed. Additional focused race tests passed for own-password changes, first-start bootstrap, and console recovery. Tests cover legacy migration, session persistence/expiry/logout, cookie attributes, CSRF rejection, role changes, password resets, and API-key isolation.
- Playwright passed on isolated preview and deployed port 8001: administrator/user refresh persistence, logout followed by reload, named administrator management, own-password change, all requested windows, password field semantics, key management, live account revocation, mobile sizing, five charts, and offline/reconnect. Idle time spanning a heartbeat produced zero data polling requests. Observed deployed visible update latency was 30 ms, not a performance guarantee.
- Verified the deployed migration against a pre-upgrade database snapshot: all existing API keys and request rows were preserved exactly. Service health passed after startup; temporary test accounts and keys were disabled/revoked.
- Deployed protocol smoke passed all eight JSON/streaming requests across Anthropic Messages, Responses, Chat Completions, and legacy Completions, with persisted token usage matching upstream responses. Accounting reported zero dropped and queued records after verification.

## Anonymous access and gateway port migration — 2026-10-29

- Added a persisted, administrator-only anonymous API access switch, disabled by default. Missing credentials can pass through when enabled; supplied invalid/revoked/conflicting keys still fail. Anonymous metadata uses a NULL key reference and is included in administrator statistics, histories, histograms, and live notifications. Ordinary-user scope remains limited to their own keys.
- Existing accounting tables migrate transactionally to nullable key references while retaining request IDs, metadata, indexes, and foreign-key enforcement. No synthetic API key is created.
- Full Go tests and focused anonymous race tests passed. Coverage includes configuration persistence, authorization/CSRF, invalid headers, anonymous JSON and streamed usage, errors, bounded nonblocking logging, database migration, statistics filters, and notification/user isolation.
- Isolated Playwright checks passed: two administrator tabs receive setting changes live, the toggle enables/disables keyless calls, anonymous requests appear in statistics, invalid keys remain rejected, ordinary users cannot change the setting or see anonymous history, and desktop/mobile layouts fit. The mode is not enabled automatically on deployment.
- Default topology changes to Model Meter on port 8000 and an upstream at `127.0.0.1:8001`. The local DS4 launcher was prepared to bind its HTTP API to loopback on 8001 while preserving its model, inference settings, and tensor-parallel transport.
