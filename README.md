# Conseil

A personal assistant that runs on exe.dev and keeps its complete execution history.

```
iPhone -> exe.dev HTTPS -> Go service -> LLM integration
                                  |
                                  +-> SQLite event log
```

The first slice supports native chat, server-side runs that continue when the app disconnects, streamed run events, and a full trace view.

## Repository

- `cmd/conseil`: Go API, worker, LLM client, and SQLite event store
- `ios/Conseil`: native SwiftUI app
- `ios/ConseilCoreTests`: decoding and event-stream tests
- `deploy`: systemd unit for exe.dev

## Trace model

Each run records append-only events. The current event types are:

- `user.message`
- `run.queued`, `run.started`, `run.completed`, `run.failed`, `run.interrupted`
- `model.request`, `model.response.started`, `model.event`
- `assistant.delta`, `assistant.message`

`model.request` contains the exact JSON sent to the model. `model.event` contains every raw provider stream event. Each run records its agent name and optional parent run, so later subagents can share one trace tree. Run rows are query projections; the event log is the audit record.

## Go service

Requirements: Go 1.25 or newer.

```sh
go test -race ./...
go vet ./...
golangci-lint run
```

Configuration:

| Variable | Default |
| --- | --- |
| `CONSEIL_ADDR` | `127.0.0.1:8000` |
| `CONSEIL_DB_PATH` | `conseil.db` |
| `CONSEIL_LLM_BASE_URL` | `https://llm.int.exe.xyz/v1` |
| `CONSEIL_AGENT_NAME` | `conseil` |
| `CONSEIL_MODEL` | `gpt-5.6-sol` |
| `CONSEIL_REASONING_EFFORT` | `high` |
| `CONSEIL_INSTRUCTIONS` | Built-in Conseil instructions |
| `CONSEIL_OWNER_EMAIL` | Required |
| `CONSEIL_RUN_TIMEOUT` | `10m` |

The service listens on loopback because exe.dev proxies `localhost:8000`. This prevents Tailscale or another direct network path from spoofing exe.dev identity headers. For local development only, set `CONSEIL_ALLOW_UNAUTHENTICATED=true`.

```sh
CONSEIL_ALLOW_UNAUTHENTICATED=true go run ./cmd/conseil
```

## iOS app

Open `ios/Conseil.xcodeproj` in Xcode 16 or newer. The deployment target is iOS 17.

The app asks for:

1. The private exe.dev URL, such as `https://yolk-adze.exe.xyz`.
2. A VM-scoped HTTPS token. Generate one locally:

```sh
ssh exe.dev ssh-key generate-api-key --vm=yolk-adze --label=conseil-ios
```

The app stores the token in Keychain and sends it in `X-Exedev-Authorization`. The server also checks the signed `X-ExeDev-Email` identity added by exe.dev.

The shared Swift code can be compiled without the app target:

```sh
cd ios
swift build --target ConseilCore
```

Running the iOS app and its tests requires a full Xcode installation.

## Deployment

Build a static Linux binary:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o conseil ./cmd/conseil
```

Install the binary and `deploy/conseil.service` on the VM. Create `/etc/conseil.env` with mode `0600`:

```sh
CONSEIL_DB_PATH=/var/lib/conseil/conseil.db
CONSEIL_OWNER_EMAIL=you@example.com
CONSEIL_MODEL=gpt-5.6-sol
CONSEIL_REASONING_EFFORT=high
```

Then enable the service:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now conseil
```

Keep the exe.dev proxy private and point it at port 8000:

```sh
ssh exe.dev share port yolk-adze 8000
ssh exe.dev share set-private yolk-adze
```

SQLite survives service and VM restarts. An encrypted off-VM backup is still required before treating the trace as irreplaceable data.
