# AGENTS.md

## Project Overview

DeskFerry is a Go + .NET + Python + Android project for an outbound-only RDP rendezvous tunnel. The normal relay is the Azure App Service at `https://test-officialwebsite.azurewebsites.net/relay/`, implemented by `relay/azure-dotnet/`. The OCI Always Free fallback relay is reachable under `http://217.142.228.117/relay/<room>` and is implemented by `relay/go/`. A protocol-compatible Python/FastAPI relay lives in `relay/python/` for alternate hosts and local testing. The work-side Windows service may connect to one or more relay room URLs at the same time, while the Windows, macOS, and Android home agents connect to ordered primary/fallback relay URL lists such as `https://test-officialwebsite.azurewebsites.net/relay/workdesk` and `http://217.142.228.117/relay/workdesk`.

## Architecture Rules

- The Azure App Service relay under `/relay/` is the normal broker.
- The OCI Go relay is a compatible alternate broker, currently deployed as `deskferry-relay.service` on `217.142.228.117`.
- A named room URL under `/relay/<room>` is the normal and only user-facing pairing configuration.
- The work agent may be configured with multiple relay room URLs simultaneously when they use the same room name, so home apps can choose any reachable relay.
- The work agent sends a persistent agent identity plus slot number on idle `agent` WebSockets; relays should replace older waiting sockets with the same identity/slot instead of counting duplicates.
- Graphical relay URL list UIs should use CRUD controls with inline or selected-row editing plus drag-to-reorder; avoid returning to semicolon-separated or multiline free-form entry for user-facing multi-value relay URLs.
- Do not reintroduce generated client files or file-based pairing artifacts for the normal path.
- The Azure relay WebSocket endpoint is `/relay/ws` for the overview room and `/relay/<room>/ws` for named rooms.
- URL-only WebSocket clients should use standard proxy environment variables by default; explicit `http://` and `https://` proxy URLs are supported, and `-proxy direct` is the bypass path.
- The home side can be the Windows home app in `home-agent/windows`, the macOS home agent in `home-agent/macos`, or the Android home app in `home-agent/android/`.
- The Android app is a home-agent client only: it listens on Android loopback and connects out to the relay; it must not become a phone-hosted relay.
- The Windows home app may keep a lightweight `home-agent` WebSocket open for dashboard presence, but RDP data uses `client` sockets.
- The macOS home agent may keep a lightweight `home-agent` WebSocket open for dashboard presence, but RDP data uses `client` sockets.
- The Android home app may also keep a lightweight `home-agent` WebSocket open for dashboard presence while its foreground service is running.
- Android relay status should use the relay `dashboard` WebSocket stream, not HTTP polling.
- The Windows home app should remain control-panel/tray-first. Console mode is debug-only.
- The Windows home app should listen on loopback by default, normally `127.0.0.1:3390`.
- `work-agent/windows/service` must remain Windows-service-first. Console mode is debug-only.
- `work-agent/windows/configurator` owns the native Windows setup/configurator GUI for the work agent service.
- Do not add stealth, anti-monitoring, or obfuscation behavior.

## Sensitive Files

Relay configs with inline PEM, generated cert directories for local relay experiments, logs, and screenshots can contain private material. Do not commit generated certs, generated local relay configs, logs containing secrets, or screenshots that expose sensitive values.

The current `.gitignore` excludes `dist/`, `bin/`, `obj/`, generated executables, and generated resource objects. Preserve that behavior.

## Build Commands

Run Go tests:

```powershell
go test ./...
```

Build Azure relay zip:

```powershell
.\build\build-azure-relay.ps1
```

Build Python relay zips:

```powershell
python -m pip install -r relay\python\requirements-dev.txt
.\build\build-python-relay.ps1
```

Build Windows/macOS/relay Go artifacts:

```powershell
.\build\build-go.ps1
```

Build Android home APK:

```powershell
.\build\build-android-home.ps1
```

When `work-agent/windows/service`, `work-agent/windows/configurator`, `home-agent/windows`, `home-agent/macos`, `relay/go`, `internal/tunnel`, or shared relay behavior changes, run `go test ./...` and `.\build\build-go.ps1`. When `relay/azure-dotnet/` changes, run `.\build\build-azure-relay.ps1`. When `relay/python/` changes, run `.\build\build-python-relay.ps1`. When `home-agent/android/` changes, run `.\build\build-android-home.ps1`.

## Tooling Notes

This repo has been built with:

- Go 1.26.x installed under `D:\Scoop`
- .NET SDK 9.x, publishing the relay as `net8.0`
- Python 3.14.x
- Android SDK under `D:\Android\Sdk`
- Gradle 9.6.x with `JAVA_HOME` set per command to `C:\Program Files\Java\jdk-25` when the global value is stale
- rsrc under `D:\Go\bin` for Windows GUI manifest resources

## Deployment

Deploy the Azure relay to the App Service backing `https://test-officialwebsite.azurewebsites.net/relay/`.

Deployable artifacts:

- `dist/azure-relay/deskferry-azure-relay.zip`
- `dist/python-relay/deskferry-python-relay.zip`
- `dist/python-relay/deskferry-python-relay-linux-cp39-vendored.zip`
- `dist/bin/deskferry-relay-linux-amd64`
- `dist/bin/deskferry-agent-windows-amd64.exe`
- `dist/bin/deskferry-agent-configurator-windows-amd64.exe`
- `dist/bin/deskferry-home-windows-amd64.exe`
- `dist/bin/deskferry-home-macos-arm64`
- `dist/bin/deskferry-home-macos-amd64`
- `dist/android/deskferry-home-android-debug.apk`

After code changes, build the relevant artifacts. The user prefers deployment after code changes. Use the authenticated browser/Kudu path for the Azure App Service when requested. For the OCI Go relay, upload `dist/bin/deskferry-relay-linux-amd64` to `/tmp/deskferry-relay-linux-amd64`, install it as `/opt/deskferry/go-relay/deskferry-relay`, restart `deskferry-relay.service`, and delete the temporary upload. Do not retain old or backup relay binaries. The OCI host also has `deskferry-relay-healthcheck.timer`, which checks local `/relay/health` every minute, restarts `deskferry-relay.service` when the app stops responding, and reboots after three consecutive failed post-restart checks. The host is also configured with a 2 GiB swap file, persistent journald, `softdog`, systemd `RuntimeWatchdogSec=60s`, and kernel panic recovery for hung tasks.

## Verification Expectations

For documentation-only changes, read the modified files back or run a lightweight command if useful.

For Go changes, run:

```powershell
go test ./...
```

For release-impacting Go changes, also run:

```powershell
.\build\build-go.ps1
```

For Azure relay changes, run:

```powershell
.\build\build-azure-relay.ps1
```

For Python relay changes, run:

```powershell
python -m pip install -r relay\python\requirements-dev.txt
.\build\build-python-relay.ps1
```

For deployed OCI relay changes, also verify:

```powershell
curl.exe --proxy http://192.9.200.25:3128 http://217.142.228.117/relay/health
```

For Android home app changes, run:

```powershell
.\build\build-android-home.ps1
```

Android lint may need uncached artifacts from `dl.google.com`; if this host cannot reach that endpoint, report the lint limitation and rely on the APK build unless a connected Android device/emulator is available.

## Coding Guidance

- Prefer existing package boundaries and helper APIs.
- Keep `internal/tunnel` focused on current protocol primitives: WebSocket dialing, proxy URL handling, role constants, and byte piping.
- Keep the Windows home app URL-first; it should store a room URL and local UI settings, not implement the broker.
- Use `apply_patch` for manual file edits.
- Avoid committing generated private key material under `dist/` or elsewhere.
