# AGENTS.md

## Project Overview

DeskFerry is a Go + .NET + Python + Android project for outbound-only RDP, WinRM, and SMB/UNC rendezvous tunnels. The normal relay is the Azure App Service at `https://test-officialwebsite.azurewebsites.net/relay/`, implemented by `relay/azure-dotnet/`. The OCI Always Free fallback relay is reachable under `http://217.142.228.117/relay/<room>` and is implemented by `relay/go/`. A protocol-compatible Python/FastAPI relay lives in `relay/python/` for alternate hosts and local testing. User-facing profiles store one room name plus ordered relay service base URLs; agents compose compatible room URLs such as `https://test-officialwebsite.azurewebsites.net/relay/workdesk` and `http://217.142.228.117/relay/workdesk` at runtime.

## Architecture Rules

- The Azure App Service relay under `/relay/` is the normal broker.
- The OCI Go relay is a compatible alternate broker, currently deployed as `deskferry-relay.service` on `217.142.228.117`.
- The normal user-facing pairing configuration is a room name plus one or more relay service base URLs; full `/relay/<room>` URLs remain runtime and legacy CLI compatibility inputs.
- The work agent may be configured with multiple relay room URLs simultaneously when they use the same room name, so home apps can choose any reachable relay.
- The work agent normally maintains one `agent-control` WebSocket per relay room URL and opens `agent-session` data sockets on demand. Legacy `agent` slots remain dual-protocol rollback compatibility only.
- Graphical relay base URL list UIs should use CRUD controls with inline or selected-row editing plus drag-to-reorder; avoid semicolon-separated or multiline free-form entry for user-facing multi-value relay bases.
- Do not reintroduce generated client files or file-based pairing artifacts for the normal path.
- The Azure relay WebSocket endpoint is `/relay/ws` for the overview room and `/relay/<room>/ws` for named rooms.
- URL-only clients should use standard proxy environment variables by default; explicit `http://` and `https://` proxy URLs are supported, and `-proxy direct` is the bypass path. On Windows, both proxy `CONNECT` and plain-HTTP fallback requests use the current identity when the proxy requests NTLM authentication; LocalSystem services use a logged-on interactive user's identity because corporate proxies commonly reject the machine account. When a configured proxy rejects WebSocket `CONNECT`, clients fall back to the relay's paired streaming `POST`/`GET` transport. A standard non-CONNECT proxy can forward that fallback only to a plain-HTTP relay; HTTPS still requires `CONNECT` to establish TLS.
- The home side can be the merged Windows app in `windows/`, the macOS home agent in `home-agent/macos`, or the Android home app in `home-agent/android/`.
- The Android app is a home-agent client only: it listens on Android loopback and connects out to the relay; it must not become a phone-hosted relay.
- The Windows home app may keep a lightweight `home-agent` WebSocket open for dashboard presence, but RDP data uses `client` sockets.
- The Windows UI must check the selected room's Work-agent availability across its configured relays before Connect or Open Remote Desktop launches the RDP app. Report an offline Work agent or an unavailable relay check immediately in the UI and do not launch Remote Desktop on failure. Keep Work status current through relay `dashboard` WebSocket snapshots and events, resynchronizing after reconnect.
- The macOS home agent may keep a lightweight `home-agent` WebSocket open for dashboard presence, but RDP data uses `client` sockets.
- The Android home app may also keep a lightweight `home-agent` WebSocket open for dashboard presence while its foreground service is running.
- Android relay status should use the relay `dashboard` WebSocket stream, not HTTP polling.
- The merged Windows app should remain control-panel/tray-first. Console mode is debug-only. `DeskFerry.exe` is the only DeskFerry Windows executable and dispatches to the interactive UI, Work service, optional Home SMB network service, setup/component-management actions, and screen-capture helper by mode.
- The Windows home app should listen on loopback by default, normally `127.0.0.1:3390`.
- SMB is a separate relay service and must use the same room password as RDP and WinRM.
- TCP forwards are relay services named `tcp-<forward port>`. The Work service owns each forward's `host:port` target (`-tcp-forward forwardPort=host:port`, or the Work Services TCP forwards card) and requires a room password; Home apps listen only on `127.0.0.1` and request a forward port, never a target. Relays only pair these services and must not open listening ports for them. Home clients must require the relay to echo a TCP forward service so older relays cannot map it to RDP.
- The optional Windows Home network component uses the synthetic `198.18.0.0/30` network and permits only TCP port 445 to the configured work-host alias. It must not become a general-purpose VPN.
- The merged Windows UI should offer the virtual network adapter as an optional component selected by default, detect existing services before presenting actions, and offer Install only when a service is absent.
- `windows/workservice` must remain Windows-service-first. Console mode is debug-only.
- `windows/workui` owns the native, optional Work service controls inside the merged executable. Its room is independent from the Home UI's selected destination: the room is editable, the installed service's relay bases are preserved internally, and relay-base controls are not shown in the Work Services window. The Work service always follows the proxy saved in the main DeskFerry UI for the Windows user whose identity it uses for integrated proxy authentication. Apply saved proxy changes through native change notifications, without polling or requiring elevation; do not expose an independent Work proxy field. Preserve established v2 data sessions while replacing control connections for a proxy change.
- Do not reintroduce separate Windows installer, configurator, Work-agent, or Home-network DeskFerry executables. The merged executable self-installs and registers the required service modes; Wintun and tun2socks remain external runtime dependencies extracted only for the optional SMB bridge.
- Screen sessions use the live protocol in `docs/screen-live-protocol.md` (`internal/screenview/live_*.go`): end-to-end encrypted with a key derived from the room password, so relays only pair and pipe the `screen` service and must not parse it. Keep the legacy screenshot stream working for old peers. Remote input is accepted only when the Work service runs with `-screen-control` and the session enabled control, and the Work PC always shows the `windows/sharebanner` banner while a screen session is open, naming AI-agent sessions as such. The AI-agent interface (`internal/screenagent`, `-agent-mcp`/`-agent-json`) must keep the computer-use action vocabulary and screenshot-pixel coordinates.
- Do not add stealth, anti-monitoring, or obfuscation behavior.
- All user interfaces (the Windows app windows, the macOS Home page, the Android Home app, and the relay dashboard) follow `docs/design-system.md`. The three relays serve one shared dashboard page and icon from `internal/relaydashboard`; edit those files rather than adding relay-specific copies.

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

Build the self-contained merged Windows executable (including the optional network component payload):

```powershell
.\build\build-windows.ps1
```

Build Android home APK:

```powershell
.\build\build-android-home.ps1
```

When `windows/`, `home-agent/macos`, `relay/go`, `internal/tunnel`, or shared relay behavior changes, run `go test ./...` and `.\build\build-go.ps1`. When `windows/networkservice`, `windows/setup`, `internal/homenetwork`, or the Windows packaging script changes, also run `.\build\build-windows.ps1`. When `relay/azure-dotnet/` changes, run `.\build\build-azure-relay.ps1`. When `relay/python/` changes, run `.\build\build-python-relay.ps1`. When `home-agent/android/` changes, run `.\build\build-android-home.ps1`.

## Tooling Notes

This repo has been built with:

- Go 1.26.x installed under `D:\Scoop`
- .NET SDK 8.x, publishing the relay as `net8.0`. On this host, the `dotnet` on `PATH` (`C:\Program Files\dotnet`) has only SDK 5.0, which fails the relay build with `NETSDK1045`. The usable SDK (8.0.425) is the per-user install under `%USERPROFILE%\.dotnet`, so put it first for the build, for example from Git Bash: `PATH="$HOME/.dotnet:$PATH" DOTNET_ROOT="$HOME/.dotnet" powershell -NoProfile -File build/build-azure-relay.ps1`, or in PowerShell set `$env:DOTNET_ROOT` and prepend it to `$env:PATH`
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
- `dist/bin/deskferry-windows-amd64.exe`
- `dist/bin/deskferry-home-macos-arm64`
- `dist/bin/deskferry-home-macos-amd64`
- `dist/android/deskferry-home-android-debug.apk`

Android APKs attached to releases must be signed with the DeskFerry Android release key (certificate SHA-256 `3937cc8abfe00eefccdcfd7a4c519ee2768008cf3bd87d1d5529e393e5d34d10`) so they upgrade existing installations. The key exists only as repository secrets, so build release artifacts with the `Release Artifacts` GitHub workflow (`gh workflow run release-artifacts.yml -f tag=<tag>`), and verify the APK certificate with `apksigner verify --print-certs` before announcing the release. Never attach a locally debug-signed APK to a release.

On this Windows host, build and install the merged Windows executable with the `-DebugWindows` switch to avoid quarantine by Symantec Endpoint Protection (SEP). Do not replace an installed debug binary with a stripped release binary unless the user explicitly requests it. Every future GitHub release must include separately named optimized and debug merged Windows executables and include both in `SHA256SUMS.txt`.

Always delete old builds and copies as part of every build, installation, and upgrade; do not leave it to the user. This covers renamed or backup executables in installation folders (for example `C:\Program Files\DeskFerry\DeskFerry.exe.<version>-old`; elevate when needed), artifacts in `dist/` that the current build did not produce, downloaded log copies, temporary deploy scripts, and staged uploads or key copies on other hosts. Keep only the current artifacts and the caches and staging folders the build scripts use (`dist/gocache`, `dist/deps/windows`, `dist/tmp/windows-dependencies`, `dist/tmp/windows-payload`, `dist/tmp/DeskFerryPayload.zip`, and the relay `publish` folders). Report what was removed.

`DeskFerry.exe` is a GUI-subsystem executable, so PowerShell's `&` call operator returns immediately without waiting for it or capturing its output, even for console-style modes such as `-update-service` and `-windows-setup -cli-action install`. In any script that runs one of these modes and then acts on the result (restarting `DeskFerryHomeNetwork`, relaunching the desktop app, deleting `DeskFerry.exe.previous`, checking hashes), launch it with `Start-Process -FilePath <exe> -ArgumentList ... -Wait -PassThru -RedirectStandardOutput <file>` and check `ExitCode` first. Otherwise the follow-up steps race the file swap: a service restarted too early comes back on the old binary and keeps the renamed copy locked.

To upgrade a remote Work PC that is reachable only through DeskFerry, such as room `p`'s `blhsing-px13`:

1. Run PowerShell on it through the local Home app's saved destination and Windows login: select the destination in the Home app, then run `"C:\Program Files\DeskFerry\DeskFerry.exe" -winrm-command-file <script.ps1> -winrm-timeout 10m`. Use a script file rather than `-winrm-command` to avoid quoting problems; the WinRM session is elevated.
2. Have the remote PC download `deskferry-windows-amd64-debug.exe` and `SHA256SUMS.txt` from the GitHub Release into `C:\ProgramData\DeskFerry\update-<version>` and stop unless the hash matches.
3. Recheck the room's relay pairs and the PC's established loopback connections (ports 3389, 445, 3390, 3391, and TCP forward ports) within 10 seconds of starting, and report what will be cut.
4. Because the WinRM session runs through the Work agent being replaced, do the swap in a one-shot scheduled task running as `SYSTEM` that logs to `C:\ProgramData\DeskFerry\update-<version>.log`. In order, the task:
   - runs `-update-service "C:\Program Files\DeskFerry\DeskFerry.exe"` with `Start-Process -Wait`;
   - restarts `DeskFerryHomeNetwork` only after that returns;
   - stops the desktop app in the user's session and relaunches it through a second one-shot task whose principal is that user with `-LogonType Interactive`, then unregisters that task;
   - deletes `DeskFerry.exe.previous`.
5. Wait for the room's `last_agent_connected_at` on the relay status to advance. Then reconnect over WinRM and verify:
   - the `Work Agent version` line in `work-agent-*.log`;
   - the installed hash;
   - that each `DeskFerry.exe` process started after the swap, and that the network log shows a fresh `virtual network ready` line with the DeskFerry adapter at metric 9999.
6. Remove the scheduled tasks, the update folder and log, and any old executable copies. If a copy is still in use, schedule it with `MoveFileEx(path, [NullString]::Value, 4)`; PowerShell turns `$null` into an empty string, which fails with error 3. Delete local helper scripts too.

After code changes, build the relevant artifacts. The user prefers deployment after code changes, but deployment and installation decisions must be made independently for every component: the merged Windows app and its Work/network service modes, macOS Home agent, Android Home agent, Azure relay, OCI relay, and Python relay. Inspect the substantive diff for each component. Upgrade a component when it contains an actual functional, reliability, security, protocol, compatibility, or packaging fix that applies to that installation. Do not upgrade or restart any component merely to synchronize or display a new version number. It is acceptable for deployed components to report different versions when their code did not otherwise change.

Immediately before stopping or upgrading any component, recheck its active RDP, WinRM, SMB, and screen connections or relay pairs. The check must occur within 10 seconds of the stop; an earlier idle result is stale. Disconnecting or transparently resuming existing connections is acceptable when the target component contains an actual applicable fix and the upgrade requires a restart. When the target component changed only by version metadata, interrupting existing connections is unacceptable and the component must not be upgraded. Record or communicate the expected connection impact before a necessary disruptive upgrade, and prefer a non-disruptive or resumable path when available.

Prefer authenticated `az webapp deploy` for the Azure App Service; use the authenticated browser/Kudu path as the fallback. The App Service is `test-officialWebSite` in resource group `OfficialWebsite`:

```powershell
az webapp deploy --resource-group OfficialWebsite --name test-officialWebSite --src-path dist\azure-relay\deskferry-azure-relay.zip --type zip --clean true --restart true
```

The restart drops every relay connection. Agents reconnect within seconds, and resumable sessions rebuild after both sides reattach. Verify that `https://test-officialwebsite.azurewebsites.net/relay/health` reports the new `version`; it can answer `The service is unavailable.` for up to a minute while the site restarts. For the OCI Go relay, SSH and SCP from the work network must use HTTP `CONNECT` through `192.9.200.25:3128`; direct port 22 is expected to time out, and the Skyhigh proxy rejects SSH tunnels (`500 handshakefailed`). The 3128 proxy only permits some hosts (it returns 403 to `172.16.9.103`), so run OCI `ssh` and `scp` on `m70-misdev` through PowerShell remoting as `tmcnet\mism70` when the local host is refused; `m70-misdev` has OpenSSH but no `ncat`; use Git for Windows' plain-`CONNECT` helper there as `-o "ProxyCommand=C:\PROGRA~1\Git\mingw64\bin\connect.exe -H 192.9.200.25:3128 %h %p"`. Keep the 8.3 short path: Windows OpenSSH cannot spawn a quoted ProxyCommand path containing spaces (`posix_spawnp: No such file or directory`). Windows PowerShell 5.1 also strips embedded double quotes from native-command arguments, so send multi-line or quoted remote shell commands to `ssh` base64-encoded (`echo <base64> | base64 -d | bash`). Log in as `opc` with the private key at `D:\downloads\ssh-key-2026-06-27.key` and use the OpenSSH option `-o "ProxyCommand=ncat --proxy 192.9.200.25:3128 --proxy-type http %h %p"`, or an equivalent plain-`CONNECT` helper, on every OCI `ssh` and `scp` command. Delete any key copy staged on another host afterwards. Upload `dist/bin/deskferry-relay-linux-amd64` to `/tmp/deskferry-relay-linux-amd64`, install it as `/opt/deskferry/go-relay/deskferry-relay`, restart `deskferry-relay.service`, and delete the temporary upload. Do not retain old or backup relay binaries. The OCI host also has `deskferry-relay-healthcheck.timer`, which checks local `/relay/health` every minute, restarts `deskferry-relay.service` when the app stops responding, and reboots after three consecutive failed post-restart checks. The host is also configured with a 2 GiB swap file, persistent journald, `softdog`, systemd `RuntimeWatchdogSec=5min`, and kernel panic recovery for hung tasks. The VM has only about 500 MB of RAM: `dnf-makecache.timer` and the PCP services and timers (`pmcd`, `pmlogger*`, `pmie*`, `pmproxy`) are disabled because their periodic memory spikes froze the host into watchdog reboots every 30 to 60 minutes. Keep them disabled and do not add memory-heavy background jobs.

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

- After making code changes, commit and push the completed, verified changes to the configured upstream branch before finishing the task unless the user explicitly asks to keep them local.
- Whenever a new versioned build is produced, create and publish the corresponding GitHub Release with the repository's complete required artifact set and checksums. Pushing the code or tag without publishing the GitHub Release is not considered complete.
- Prefer existing package boundaries and helper APIs.
- Keep `internal/tunnel` focused on current protocol primitives: WebSocket dialing, proxy URL handling, v2 control messages, role constants, resumption, and byte piping.
- Keep the merged Windows app relay-first; it should store relay service bases, a profile room name, and local UI settings, not implement the broker.
- Use `apply_patch` for manual file edits.
- Avoid committing generated private key material under `dist/` or elsewhere.
