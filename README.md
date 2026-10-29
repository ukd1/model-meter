# Model Meter

Go gateway and dashboard for an OpenAI/Anthropic-compatible upstream. Both run on **port 8000**; the upstream defaults to **http://127.0.0.1:8001**. Bind the upstream to loopback so clients reach it through the gateway. SQLite stores users, hashed credentials, and request metadata.

## Install with an AI agent

Give your agent this prompt:

> Install https://github.com/ukd1/model-meter on this machine. Read and follow AGENTS.md in the repository. Use the existing OpenAI/Anthropic-compatible server at http://127.0.0.1:8001 as the upstream and run the proxy and dashboard on port 8000. Preserve any existing configuration and data. Verify authentication, streaming, usage accounting, and persistent service startup, then tell me the dashboard URL and how to retrieve the initial administrator username and password.

Adjust the upstream and port in the prompt for your machine. The [agent installation guide](AGENTS.md) includes prerequisites, service configuration, updates, and verification.

## Use

Open `http://<server>:8000` and sign in with your username and password. On initial setup, the service creates an administrator account and prints its generated password once to its startup console (the service journal under systemd). Save it in your password manager. Passwords are stored as salted hashes; there is no master administrator token.

Administrators create named accounts with the User or Administrator role in **Access → Users**. Each new account receives a generated password shown once. Users manage their own API keys, profile, and usage. Administrators manage all accounts, keys, and aggregate usage, and can promote or demote accounts with **Make administrator / Make user**. Each administrator signs in under their own username. Disabling an account also blocks its API keys. Existing API keys and usage history are preserved when changing account roles or passwords.

Each account can change its own password under **Access → Your profile** by providing the current password and a new password of at least 12 characters. Administrators can reset another account's password in the dashboard. Local recovery is available with `./model-meter reset-password USERNAME`, using the same `DATA_DIR` as the service. The command prints the new password once and invalidates existing sessions. There is no email reset flow.

Dashboard sign-in uses a separate, opaque HttpOnly session cookie that survives page refreshes and browser restarts for up to 30 days. **Lock** revokes that session. Disabling an account or resetting its password invalidates its sessions. Passwords are not saved in browser storage. For 1Password, save a Login item for the dashboard URL with your username and password in their corresponding fields.

When upgrading from token sign-in, existing accounts, API keys, and usage history are preserved. Existing accounts receive a temporary username such as `user-2` (their database ID) and need a password reset by an administrator or the local recovery command before signing in. Administrators can use **Change username** to choose a permanent login name. Existing API keys continue to work. The previous master token is no longer accepted.

Configure your OpenAI-compatible client:

```sh
export OPENAI_BASE_URL=http://<server>:8000/v1
export OPENAI_API_KEY=mm_<key-created-in-dashboard>
curl "$OPENAI_BASE_URL/chat/completions" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"Hello"}],"max_tokens":32}'
```

## Anonymous API access

Administrators can enable **Access → Anonymous API access** to allow `/v1` requests without a key. It is disabled by default, and the setting survives service restarts. Use **Disable** to require keys again for new requests. Dashboard access always requires an account.

With this mode enabled, omit both `Authorization` and `x-api-key`. Supplied credentials are still validated: invalid, revoked, disabled-account, malformed, or conflicting keys are rejected rather than treated as anonymous. Clients that insist on sending an API key should use a valid dashboard-issued key.

Anonymous requests use the same asynchronous metadata and token accounting as keyed requests. Administrators see them as **Anonymous** in request history, aggregate usage, and charts; select **Anonymous (no key)** in the key filter to see only that traffic. Ordinary users continue to see only their own keyed traffic. Anonymous history remains available after the mode is disabled; no account or synthetic key is created for it.

## API compatibility

| Method | Route | Format |
| --- | --- | --- |
| GET | `/v1/models` | Model list |
| GET | `/v1/models/{model}` | Individual model lookup |
| POST | `/v1/chat/completions` | OpenAI Chat Completions, JSON or SSE |
| POST | `/v1/responses` | OpenAI Responses, JSON or SSE |
| POST | `/v1/completions` | Legacy OpenAI Completions, JSON or SSE |
| POST | `/v1/messages` | Anthropic Messages, JSON or SSE |
| POST | `/v1/embeddings` | OpenAI embeddings, when supported upstream |

These cover DS4's model and inference routes. Upstream support determines which routes work; the gateway forwards each protocol without converting it to another API. DS4 does not expose embeddings, Anthropic token counting, or stored Responses retrieval/deletion in the inspected route table, so those are not claimed as DS4 features. The gateway does not implement token counting or stored-response routes.

OpenAI clients use base URL `http://<server>:8000/v1`. Anthropic clients use base URL `http://<server>:8000` and the same dashboard-issued client keys via `x-api-key` or `Authorization: Bearer`. Conflicting credentials are rejected. Example:

```sh
export ANTHROPIC_BASE_URL=http://<server>:8000
export ANTHROPIC_API_KEY=mm_<key-created-in-dashboard>
curl "$ANTHROPIC_BASE_URL/v1/messages" \
  -H "x-api-key: $ANTHROPIC_API_KEY" \
  -H 'anthropic-version: 2023-06-01' \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4.1-flash","max_tokens":32,"messages":[{"role":"user","content":"Hello"}]}'
```

The proxy forwards `anthropic-version`, `anthropic-beta`, and `openai-beta` protocol headers. When `UPSTREAM_API_KEY` is configured, it replaces the client credential: Anthropic Messages and requests authenticated with `x-api-key` use upstream `x-api-key`; other requests use upstream Bearer authentication. Request IDs and retry-after headers are preserved. API errors retain upstream status and response body.

Streaming is forwarded incrementally. For streaming Chat/legacy Completions, the gateway sets `stream_options.include_usage=true`; other JSON requests are passed through unchanged. Usage extraction handles Chat/Completions and Responses envelopes, plus Anthropic's initial `message_start` and cumulative `message_delta` counters. Anthropic total input is normalized as `input_tokens + cache_read_input_tokens + cache_creation_input_tokens`; cached input displays cache-read tokens. This keeps input totals comparable with OpenAI usage without double-counting stream snapshots. See the [Anthropic streaming protocol](https://platform.claude.com/docs/en/build-with-claude/streaming).

## Accounting and privacy

Persisted request fields: key ID, UTC start time, model, method, known endpoint label, direct peer IP, HTTP status, total duration in milliseconds, input/output/cached tokens, and completion/interruption outcome. No request/response bodies, arbitrary headers, authorization values, or query strings are logged. Bodies are inspected transiently in memory to extract model and usage. Model names and administrator/user supplied names are intentional metadata. Client keys are replaced with the optional upstream credential; client headers are not blindly forwarded.

Completed metadata enters a bounded **8192-record queue without waiting**. A background writer commits immediately when records arrive, draining up to 100 records already queued into each transaction. It never waits for a timer or a full batch. WAL with a connection pool lets authentication reads run alongside accounting writes. Graceful shutdown drains handlers and the queue. Crashes can lose queued records; a full queue or failed transaction drops accounting records instead of blocking responses. The dashboard warns about dropped records, and `/admin/status` exposes `dropped_records` and `queued_records` (dropped count resets on restart). This is usage monitoring, not billing-grade accounting.

Totals use upstream-reported usage. Missing usage is NULL and shown as unmetered, never estimated. Interrupted streams may lack final usage. Cached tokens are included in input totals. Metadata persists indefinitely; back up SQLite with its backup API or while the service is stopped. The live charts cover the selected window (last 1, 5, or 15 minutes; 1, 4, 12, or 24 hours; 7 or 30 days; or all time) and honor user/key/model filters. Time-bucket sizes adapt to keep roughly 120 points or fewer. Completed accounting batches and account/key/settings changes trigger live updates through an authenticated Server-Sent Events connection; the browser does not poll for data. Recent requests are limited to the latest 200; aggregates cover all matching history.

Request bodies are limited to 32 MiB; metering inspection buffers are limited to 8 MiB per JSON response/SSE line. Oversized or unrecognized usage envelopes are unmetered. Requests have a 30-minute upstream deadline. The service records authenticated requests and, when enabled, anonymous requests with no key ID; rejected authentication is not recorded as anonymous traffic. Existing in-flight requests can finish after a key is revoked.

## Operate

```sh
go test -race ./...
go vet ./...
go build -o model-meter .
systemctl --user status model-meter
systemctl --user restart model-meter
journalctl --user -u model-meter
```

The supplied user service assumes the checkout is at `$HOME/model-meter`. Systemd uses `%h` to expand the user's home directory in unit files; it does not use shell-style `$HOME` expansion for these paths. For a different checkout location, adjust the paths in the unit before installing it. Running `./model-meter` directly uses a `data/` directory relative to the current working directory unless `DATA_DIR` is set.

Install the service from the project directory after building:

```sh
mkdir -p ./data "$HOME/.config/systemd/user"
chmod 700 ./data
cp ./deploy/model-meter.service "$HOME/.config/systemd/user/model-meter.service"
systemctl --user daemon-reload
systemctl --user enable --now model-meter
```

The installed unit is in `~/.config/systemd/user/model-meter.service`; the repository copy is `deploy/model-meter.service`. Configuration: `LISTEN_ADDR`, `UPSTREAM_URL` (origin without `/v1`), `DATA_DIR`, and optional `UPSTREAM_API_KEY`. Service overrides can be placed in ignored `data/service.env` with mode 0600. Upstream credentials are not managed in the UI: its keys identify callers of this gateway.

The local deployment uses HTTP, like the existing upstream. Use a trusted network or SSH tunnel (`ssh -L 8000:localhost:8000 <user>@<server>`) for access; terminate TLS before exposing it to an untrusted network. Forwarded IP headers are deliberately not trusted. No external frontend dependencies, fonts, or analytics are loaded.

## Live dashboard

The interface separates Overview, Requests, and Access. It uses local fonts, a minimal terminal-inspired green/violet palette, and reduced-motion-aware effects; all frontend assets are embedded in the Go binary.

`GET /admin/events` opens an authenticated SSE stream. The browser authenticates with its HttpOnly session cookie, never a credential in the URL or browser storage. Administrative clients first send JSON `{ "username": "…", "password": "…" }` to `POST /auth/login` with `X-Model-Meter: dashboard` and retain the returned session cookie. Dashboard routes do not accept a master token or client API key. Cookie-authenticated dashboard changes require the dashboard request header and same-origin requests. On a committed usage batch or account/key/settings change, the server sends a notification and the browser immediately reloads its authorized view, with no fixed browser debounce. Overlapping refreshes are coalesced. Changes are coalesced into a bounded one-slot notification queue per connection. A slow dashboard cannot block accounting writes or inference responses.

Users receive notifications only for their own activity; administrators receive all activity. User access is revalidated before notifications and 15-second keepalive heartbeats. Disabling a user or resetting their password closes their live session. Network interruptions trigger reconnect with backoff and a fresh snapshot to recover missed changes. Heartbeats keep the stream alive; they do not trigger periodic data fetches. The selected time window is remembered in this browser across page refreshes when browser storage is available. Usage appears when a request finishes and its metadata batch commits, not token by token during inference.

The timeline advances locally every two seconds while Overview is visible, even without traffic. It uses the server snapshot time plus elapsed browser time; these redraws make no HTTP requests. Hidden tabs and Access suspend the clock. For bounded windows, the server supplies the next matching record's expiry time; the browser fetches a new snapshot at that deadline so totals, histograms, and the request list age correctly. Failed expiry refreshes retry with backoff. Empty windows need no expiry fetches. Returning to a hidden tab or changing filters also refreshes the data.

If placing another reverse proxy in front, disable response buffering for `/admin/events` and permit long-lived HTTP streams. The service sends `X-Accel-Buffering: no` and bounds each stream write with a deadline. Graceful service shutdown closes dashboard streams before draining inference handlers.

## Performance charts

`GET /admin/performance` computes distributions and time series over all matching records, independently of the 200-row recent-request limit. It uses the same user authorization and time/key/model/user filters as the usage views. Summary queries share one SQLite read transaction for a consistent snapshot.

- **Latency histogram:** full request duration in seconds for successful, completed Chat Completions, Responses, Messages, and legacy Completions requests.
- **Output-speed histogram:** provider-reported output tokens divided by total request duration in seconds. This includes queueing, prompt processing, generation, and delivery; it is **end-to-end output speed, not decode-only speed**. Missing output usage and zero-duration records are excluded, while a measured zero-token response remains a valid zero-speed sample.
- **Output-speed graph:** arithmetic mean of eligible per-request output speeds in each time bucket; gaps indicate no eligible samples, not zero speed.
- **Request/token flow graphs:** per-minute average rates within each adaptive bucket. These measure aggregate activity, distinct from per-request output speed.

Histograms show request counts in labeled, lower-inclusive/upper-exclusive ranges; the final range is open-ended. p50/p95 use the nearest-rank definition. Model lookups, failures, interrupted requests, and embeddings are excluded from performance distributions. All request types still count toward traffic and usage totals. Bucket assignment and window filtering use request start time, and graphs reflect completed records as of the snapshot timestamp. No token arrival timestamps or request/response bodies are stored. Historical records work without a schema migration.

## Browser verification

`tests/browser.cjs` exercises the deployed dashboard with Playwright. It creates a QA user/key, tests both roles, checks graphs and mobile sizing, verifies pushed updates without idle polling, exercises network reconnection and cross-session key changes, then revokes the key and confirms that disabling the user closes their active session. It requires Node 20+ and Playwright with Chromium installed; supply `NODE_PATH` if Playwright is installed outside this repository. `TEST_URL`, `ADMIN_USERNAME`, and `ADMIN_PASSWORD` override its defaults. It writes screenshots under `/tmp`, without full credentials visible.

`TEST_URL=http://127.0.0.1:8000 node tests/chart_clock.cjs` tests the moving window using Playwright's browser clock and mocked administrative responses. It checks idle scrolling without data polling, record expiry, clock skew, hidden views, window changes, sparse speed samples, and expanding all-time history. It needs the same Playwright installation, but no credentials; it does not create accounts or send inference requests.

Streaming usage follows the [OpenAI streaming event format](https://developers.openai.com/api/reference/resources/chat/subresources/completions/streaming-events).

## Live protocol verification

Run `python3 tests/protocol_smoke.py` from the checkout to test model discovery and all four inference protocols in regular and streaming modes against a running DS4-backed proxy. It compares upstream usage with persisted accounting and revokes its temporary client key in cleanup. It runs eight short inference requests; response bodies and credentials are never written to disk. Override `TEST_URL`, `ADMIN_USERNAME`, and `ADMIN_PASSWORD` for another deployment. Other upstreams may implement only a subset of the tested routes.
