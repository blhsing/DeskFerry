<img src="home-agent/windows/app-icon-256.png" alt="DeskFerry icon" width="256">

# DeskFerry

DeskFerry is an outbound-only RDP, WinRM, and SMB rendezvous tunnel for a work PC that cannot accept inbound connections. The current architecture uses an Azure App Service relay at `https://test-officialwebsite.azurewebsites.net/relay/` and an OCI Always Free fallback relay at `http://217.142.228.117/relay/`. The Azure relay implementation is .NET, the OCI relay implementation is a lightweight Go service, and a protocol-compatible Python/FastAPI relay is also available under `relay/python/`. The work-side Windows service and the Windows, macOS, and Android home agents connect out to relay web services over WebSockets.

Home apps accept one or more relay room URLs in priority order. The first URL is the primary relay; later URLs are fallbacks used when the primary cannot connect or cannot pair an RDP stream. The Windows and Android home apps organize those same-room URL lists into named work-destination profiles, so a user can choose among multiple work PCs without mixing different rooms into one fallback list. Existing single-list settings migrate automatically to a `Work` destination. The Windows, Android, and work-agent configurator UIs manage relay URLs as ordered lists with add, edit, delete, and reorder controls. The work agent can connect to one or more relay room URLs at the same time, as long as they use the same room name. For example:

```text
https://test-officialwebsite.azurewebsites.net/relay/workdesk
http://217.142.228.117/relay/workdesk
```

The room name is the path segment after `/relay/`. The first work agent to use a room creates it in memory on that relay. Rooms may be left unprotected or protected with a password configured on the work agent; protected home clients must supply the same password.

The Android app is a home-agent client like the Windows and macOS home agents. It is not a phone-hosted relay service.

## Table Of Contents

- [Release Notes](CHANGELOG.md)
- [How It Works](#how-it-works)
- [Installation](#installation)
  - [1. Deploy Azure Relay](#1-deploy-azure-relay)
  - [2. Deploy Go Relay On OCI](#2-deploy-go-relay-on-oci)
  - [3. Choose A Room URL](#3-choose-a-room-url)
  - [4. Install Work Agent](#4-install-work-agent)
  - [5. Run Windows Home App](#5-run-windows-home-app)
  - [6. Run macOS Home Agent](#6-run-macos-home-agent)
  - [7. Run Android Home App](#7-run-android-home-app)
- [Deliverables](#deliverables)
  - [Azure Relay Web Service](#azure-relay-web-service)
  - [Go Relay Web Service](#go-relay-web-service)
  - [Python Relay Web Service](#python-relay-web-service)
  - [Work Agent](#work-agent)
  - [Agent Configurator](#agent-configurator)
  - [Windows Home App](#windows-home-app)
  - [macOS Home Agent](#macos-home-agent)
  - [Android Home App](#android-home-app)
- [Security Model](#security-model)
- [Build Prerequisites](#build-prerequisites)
- [Build Commands](#build-commands)
- [URL Configuration](#url-configuration)
- [Troubleshooting](#troubleshooting)
  - [Diagnostic Logs](#diagnostic-logs)
  - [Repeated RDP Disconnects After A Network Drop](#repeated-rdp-disconnects-after-a-network-drop)
  - [Agent Self-Test Fails Through Proxy](#agent-self-test-fails-through-proxy)
  - [Endpoint Protection Flags Windows Binaries](#endpoint-protection-flags-windows-binaries)
  - [Home App Connects But RDP Fails](#home-app-connects-but-rdp-fails)
  - [Work File Access Fails](#work-file-access-fails)
  - [OCI Relay Becomes Unresponsive](#oci-relay-becomes-unresponsive)
  - [Saved Windows Login](#saved-windows-login)
  - [Azure Relay Status](#azure-relay-status)
- [Development](#development)
- [Repository Layout](#repository-layout)
- [Status](#status)
- [Current Limitations](#current-limitations)

## How It Works

```text
Home PC, Mac, or Android device
  RDP client -> 127.0.0.1:<home agent port>
        |
        v
DeskFerry home agent
  Windows GUI, macOS CLI, or Android foreground service
  outbound WebSocket over HTTPS
        |
        v
Relay web service
  Azure: https://test-officialwebsite.azurewebsites.net/relay/workdesk
  OCI:   http://217.142.228.117/relay/workdesk
        |
        v
agent.exe Windows service
  outbound WebSockets to one or more relay services, optionally through an HTTP or HTTPS proxy
  RDP sockets   -> 127.0.0.1:3389
  WinRM sockets -> 127.0.0.1:5985 (when enabled)
  SMB sockets   -> 127.0.0.1:445 (when enabled)
```

The relay groups sockets by room name and service type. A waiting work-agent socket is paired with one authenticated home-client socket for RDP, WinRM, or SMB in the same room, then the relay copies binary WebSocket frames in both directions. The relay stores only an in-memory room-scoped password proof, never the room password or Windows login credentials.

On Windows Home PCs, the optional `DeskFerryHomeNetwork` service creates a Wintun Layer-3 adapter for the synthetic `198.18.0.0/30` network. The installer maps `deskferry-work` to `198.18.0.2`; tun2socks sends that adapter's TCP stream to a DeskFerry-owned loopback SOCKS endpoint, which accepts only the synthetic address on TCP port 445. The work agent then connects to the work PC's existing loopback SMB server. Normal Internet and LAN routes are not changed.

Current agents negotiate resumable RDP streams with the relay. If an HTTP proxy or network path drops an active WebSocket, both endpoints keep their local RDP sockets open, reconnect to the same relay session for up to five minutes, and replay only data that the peer has not acknowledged. Each endpoint buffers at most 8 MiB of unacknowledged data so an extended outage applies backpressure instead of consuming unbounded memory. Older agents and relays continue to use the original non-resumable stream protocol.

Resumption is enabled only when both paired endpoints send `X-DeskFerry-Resumable: 1`. The relay then returns a random session ID in `start <session-id>`. Following an abnormal transport close, each endpoint reconnects with the `resume` role, the session ID, and its `agent` or `client` side; the relay reattaches both sockets to the existing logical pair. A normal WebSocket close still ends the RDP session immediately. Relay restarts cannot preserve sessions because resume state is intentionally in memory.

The home app also keeps a lightweight `home-agent` presence WebSocket open while it is running. That presence socket lets the relay dashboard and home control panels show whether the home side is online; RDP data still flows only when a home agent starts a local listener and an RDP client connects to it.

The Android home app follows the same model. It runs a foreground service, listens on Android loopback, and lets a separate Android RDP client connect to `127.0.0.1:<port>` on the phone. The phone is not a relay and does not need inbound access from the internet.

## Installation

### 1. Deploy Azure Relay

Build the deployable zip:

```powershell
.\build\build-azure-relay.ps1
```

Deploy `dist\azure-relay\deskferry-azure-relay.zip` to the Azure App Service with Azure CLI:

```powershell
az webapp deploy --resource-group OfficialWebsite --name test-officialWebSite --src-path .\dist\azure-relay\deskferry-azure-relay.zip --type zip --clean true --restart true --track-status true
```

The authenticated Kudu Zip Deploy page is the fallback when Azure CLI is unavailable. Confirm WebSockets are enabled in App Service configuration. On App Service, the relay writes directly to `%HOME%\LogFiles\Application\deskferry-relay-<instance>-<pid>.log`; this avoids depending on ANCM stdout capture.

Dashboard and health endpoints:

```text
https://test-officialwebsite.azurewebsites.net/relay/
https://test-officialwebsite.azurewebsites.net/relay/<room>
https://test-officialwebsite.azurewebsites.net/relay/health
https://test-officialwebsite.azurewebsites.net/relay/status
```

### 2. Deploy Go Relay On OCI

The OCI fallback relay runs as a small native Go binary. The current OCI deployment is:

```text
http://217.142.228.117/relay/b
```

It runs as the systemd service `deskferry-relay.service` under `/opt/deskferry/go-relay` and listens on public HTTP port `80`. The OCI security rules must allow inbound TCP `80`, and the VM firewall must allow the `http` service.

The OCI relay does not terminate TLS. Use `http://217.142.228.117/relay/<room>`, not `https://217.142.228.117/relay/<room>`. The `https://` form makes clients try port `443`, which is not served by this VM.

Build the native Linux relay binary:

```powershell
.\build\build-go.ps1
```

On Windows hosts where endpoint protection falsely classifies stripped Go PE files, build the Windows deliverables with symbols and compiler optimizations disabled:

```powershell
.\build\build-go.ps1 -DebugWindows
```

Artifact:

```text
dist\bin\deskferry-relay-linux-amd64
```

SSH egress from the work network must go through the HTTPS proxy. Direct `ssh` and `scp` to OCI are expected to time out. Use Ncat as OpenSSH's HTTP `CONNECT` transport:

```powershell
$ociKeyPath = "$env:USERPROFILE\Downloads\ssh-key-2026-06-27.key"
$ociProxyCommand = 'ncat --proxy 192.9.200.25:3128 --proxy-type http %h %p'
ssh -i $ociKeyPath -o IdentitiesOnly=yes -o "ProxyCommand=$ociProxyCommand" opc@217.142.228.117 'hostname'
```

Deploy through a temporary upload, replace the installed binary, restart the service, and remove the upload so old or staging binaries are not retained:

```powershell
scp -i $ociKeyPath -o IdentitiesOnly=yes -o "ProxyCommand=$ociProxyCommand" dist/bin/deskferry-relay-linux-amd64 opc@217.142.228.117:/tmp/deskferry-relay-linux-amd64
ssh -i $ociKeyPath -o IdentitiesOnly=yes -o "ProxyCommand=$ociProxyCommand" opc@217.142.228.117 'sudo install -m 0755 /tmp/deskferry-relay-linux-amd64 /opt/deskferry/go-relay/deskferry-relay && sudo systemctl restart deskferry-relay.service && sudo rm -f /tmp/deskferry-relay-linux-amd64'
```

Keep the proxy command on every OCI SSH/SCP operation from this network; the proxied health check alone does not imply that direct SSH is available.

The current OCI host is hardened for a small Always Free VM: it uses a 2 GiB swap file, persistent journald, a five-minute systemd runtime watchdog through `softdog`, kernel panic recovery for hung tasks, and a local health timer named `deskferry-relay-healthcheck.timer`. The longer watchdog window avoids resetting a resource-constrained VM during short hypervisor stalls while still recovering sustained hangs. The timer checks `http://127.0.0.1/relay/health` every minute, restarts `deskferry-relay.service` when the relay process stops responding, and reboots the VM after three consecutive failed post-restart checks.

### 3. Choose A Room URL

Pick a room name that is easy for you to remember but not obvious to outsiders:

```text
https://test-officialwebsite.azurewebsites.net/relay/workdesk
```

For the OCI relay, the equivalent room URL is:

```text
http://217.142.228.117/relay/workdesk
```

Keep the `http://` scheme for OCI room URLs. If a home or work log shows `https://217.142.228.117/...`, that client is trying port `443` and will fail before it reaches the relay.

Use the same room name everywhere. The work agent can use both URLs at the same time. Home apps can also use both URLs as an ordered primary/fallback list, with the first row treated as primary and later rows treated as fallbacks.

### 4. Install Work Agent

Run the configurator:

```text
deskferry-agent-configurator-windows-amd64.exe
```

It defaults to `D:\DeskFerry\Agent` when `D:` exists. Select `deskferry-agent-windows-amd64.exe`, manage one or more relay room URLs in the ordered URL list, optionally enter a room password, then click `Install / Update`. One password protects every configured relay room. The configurator encrypts it with machine-scope Windows DPAPI, copies the work agent as `agent.exe`, installs or updates the automatic `DeskFerryAgent` Windows service, configures SCM restart recovery, and starts the service.

To enable remote command execution, set the WinRM target to `127.0.0.1:5985`. WinRM requires a non-empty room password. The target can be changed for an existing local WinRM configuration, but it must remain a `host:port` reachable by the work service.

To enable Windows file sharing, set the SMB target to `127.0.0.1:445`, leave the SMB server alias at `deskferry-work`, and use a non-empty room password. The configurator registers that specific alias with the Windows Server service; restart the work PC once if the alias is not accepted immediately. Create and permission shares with the normal Windows **Advanced Sharing** controls. DeskFerry does not create shares or weaken their NTFS/share permissions.

The configurator also exposes every setup field and service action through its CLI. Run it from an elevated PowerShell session when the caller must wait for completion; otherwise it requests UAC elevation and returns after launching the elevated action. Supply passwords over standard input so they do not appear in the process command line:

```powershell
Read-Host "Room password" -MaskInput | .\deskferry-agent-configurator-windows-amd64.exe `
  -cli-action install `
  -install-dir 'C:\Program Files\DeskFerry\Agent' `
  -agent .\deskferry-agent-windows-amd64.exe `
  -relay-url https://test-officialwebsite.azurewebsites.net/relay/workdesk `
  -relay-url http://217.142.228.117/relay/workdesk `
  -room-password-stdin `
  -winrm 127.0.0.1:5985 `
  -smb 127.0.0.1:445 `
  -smb-alias deskferry-work
```

Omit all password flags during an update to preserve the installed DPAPI-protected credential. Use `-clear-room-password` to remove it, or `-room-password-blob <path>` to install and consume an existing machine-scope DPAPI blob. The available `-cli-action` values are `install`, `start`, `stop`, `restart`, `uninstall`, and `status`; run the configurator with `-cli-help` for the complete option summary.

The work agent's narrower command-line installer is also supported:

```powershell
.\deskferry-agent-windows-amd64.exe -install -relay-url https://test-officialwebsite.azurewebsites.net/relay/workdesk
.\deskferry-agent-windows-amd64.exe -install -relay-url https://test-officialwebsite.azurewebsites.net/relay/workdesk -relay-url http://217.142.228.117/relay/workdesk
```

Useful checks:

```powershell
.\deskferry-agent-windows-amd64.exe -status
.\deskferry-agent-windows-amd64.exe -self-test -relay-url https://test-officialwebsite.azurewebsites.net/relay/workdesk
.\deskferry-agent-windows-amd64.exe -self-test -relay-url https://test-officialwebsite.azurewebsites.net/relay/workdesk -relay-url http://217.142.228.117/relay/workdesk
```

WebSocket mode uses standard proxy environment variables by default, such as `HTTP_PROXY` and `HTTPS_PROXY`. Use `-proxy http://proxy.example:8080` or `-proxy https://proxy.example:8443` to force a proxy, or `-proxy direct` to bypass proxy discovery. For plain `http://` relay URLs behind a corporate proxy, DeskFerry opens a `CONNECT` tunnel first so the WebSocket upgrade reaches the relay unchanged.

The work agent writes persistent daily diagnostic logs under `%ProgramData%\DeskFerry`. Seven calendar days are retained by default. Use `-log-retention-days <days>` in the service command line to configure a value from 1 through 3650.

### 5. Run Windows Home App

The preferred installation is the self-contained setup application:

```text
dist\windows-home-installer\DeskFerryHomeSetup.exe
```

**Enable `\\deskferry-work\...` file access with the DeskFerry virtual network adapter** is selected by default and can be cleared for an app-only installation. When selected, enter the same relay room URL list and room password as the work agent. Setup installs the Home GUI, Start menu and Apps & Features entries, the automatic restricted network service, signed Wintun, and tun2socks. When it is cleared during an update, setup removes the network service, adapter helpers, and managed hostname while leaving the Home GUI installed.

Setup doubles as the installed Home configurator. When reopened, its UI and CLI load the installed location plus the current relay list, proxy, alias, and adapter state. Setup preserves the protected derived room proof during an elevated same-room update, so the password is not required again; changing rooms or enabling file access without an existing proof does require it. A complete non-interactive install or reconfiguration can be run from an elevated PowerShell session:

```powershell
Read-Host "Room password" -MaskInput | .\DeskFerryHomeSetup.exe `
  -cli-action configure `
  -relay-url https://test-officialwebsite.azurewebsites.net/relay/workdesk `
  -relay-url http://217.142.228.117/relay/workdesk `
  -proxy direct `
  -alias deskferry-work `
  -enable-network=true `
  -room-password-stdin
```

The CLI actions are `install`, `configure`, `uninstall`, and `status`. Use `-enable-network=false` for an app-only configuration, `-room-password-blob <path>` to read an existing machine-scope DPAPI room-password blob, and `-cli-help` for all options. From a non-elevated shell, Setup requests UAC elevation and returns after launching the elevated action.

After both sides are configured, open an existing work share in Explorer:

```text
\\deskferry-work\sharename
```

Use the Windows account that has permission to that share. A domain environment may fall back to NTLM for the friendly alias; organizations that require Kerberos-only SMB should register the alias and `cifs/<alias>` SPN through their domain administrators or configure the actual work hostname as the alias.

The standalone Home binary remains available for portable/manual use:

Start the Windows home app, choose or create a named destination, and manage that destination's relay room URLs in priority order. The first URL is primary; later URLs are fallbacks. Stop the tunnel before changing destinations:

```powershell
.\deskferry-home-windows-amd64.exe -relay-url https://test-officialwebsite.azurewebsites.net/relay/workdesk
.\deskferry-home-windows-amd64.exe -relay-url https://test-officialwebsite.azurewebsites.net/relay/workdesk -relay-url http://217.142.228.117/relay/workdesk
```

The app opens a friendly control panel and a notification-area icon. Enter the same room password once for the selected destination, then click `Connect` to start the local listeners and open Remote Desktop. The default RDP listener is `127.0.0.1:3390`, avoiding Windows' normal local RDP port `3389`. When a room credential is saved, the app also listens on `127.0.0.1:3391` for WinRM and opens one outbound WebSocket to the first reachable relay for each local connection.

The **WinRM Commands** panel executes a PowerShell command on the work host using the same Windows username and password as RDP. Each named destination keeps its own username and optional shared login in Windows Credential Manager; **Save Windows Login** and **Forget Windows Login** affect both RDP and WinRM for that destination. Passwords are never written to the JSON profile. The work host must have WinRM enabled and allow the supplied account.

Only one Windows home-app instance runs on the machine. Launching it again restores and focuses the existing control panel when it is in the same interactive session, instead of creating another tray icon, relay presence connection, or local listener.

The home app stores its destination profiles, usernames, room URL lists, local listener addresses, and proxy mode in `%APPDATA%\DeskFerry\home-client.json`. Saved Windows passwords remain in Windows Credential Manager. Console debug mode is still available:

```powershell
.\deskferry-home-windows-amd64.exe -console -relay-url https://test-officialwebsite.azurewebsites.net/relay/workdesk
```

Persistent diagnostic logs are written under `%APPDATA%\DeskFerry`. Their default retention is seven calendar days; use `-log-retention-days <days>` to configure a value from 1 through 3650.

### 6. Run macOS Home Agent

Choose the binary for your Mac:

```sh
chmod +x ./deskferry-home-macos-arm64
./deskferry-home-macos-arm64 -relay-url https://test-officialwebsite.azurewebsites.net/relay/workdesk -room-password 'strong password' -open-rdp
./deskferry-home-macos-arm64 -relay-url https://test-officialwebsite.azurewebsites.net/relay/workdesk -relay-url http://217.142.228.117/relay/workdesk -open-rdp
```

Use `deskferry-home-macos-amd64` on Intel Macs. The macOS home agent runs in the foreground, listens on `127.0.0.1:3389` by default, keeps the relay dashboard presence socket connected, and opens an `.rdp` profile when `-open-rdp` is supplied. If your RDP app does not open automatically, connect it manually to:

```text
127.0.0.1:3389
```

The macOS agent has the same persistent daily diagnostics and `-log-retention-days <days>` option as the Windows home agent. Logs are stored in the user's DeskFerry configuration directory, normally `~/Library/Application Support/DeskFerry`.

### 7. Run Android Home App

Install the debug-signed APK:

```text
dist\android\deskferry-home-android-debug.apk
```

Open DeskFerry Home, keep the local RDP port at `3389`, and choose or create a named destination. Each destination stores the same relay room URLs and optional room credential as one work agent in its own ordered URL list. Enter the same room password before starting a protected destination. Stop the tunnel before changing destinations. In an Android RDP client, connect to:

```text
127.0.0.1:3389
```

The Android app keeps the tunnel alive through a foreground service while you switch to the RDP client. It maintains the same `home-agent` presence socket used by the relay dashboard and a `dashboard` WebSocket for live relay status updates. Its Proxy field accepts `system`, `direct`, `http://host:port`, or `https://host:port`; optional Basic credentials can be included in the proxy URL.

Android writes the same daily diagnostics to the app-specific external-files `logs` directory, falling back to internal app storage when necessary. Set **Diagnostic log retention days** in the control panel; the default is 7 and the accepted range is 1 through 3650. The activity log prints the resolved diagnostic-log path when the foreground service starts.

## Deliverables

### Azure Relay Web Service

`relay/azure-dotnet/` is a .NET 8 minimal ASP.NET Core service. It exposes:

- `GET /relay/` for the live overview dashboard.
- `GET /relay/{room}` for a room-scoped live dashboard.
- `GET /relay/health` for machine-readable health.
- `GET /relay/status` for JSON status.
- `GET /relay/ws` and `GET /relay/{room}/ws` as WebSocket endpoints.

WebSocket clients identify their role with:

```text
X-DeskFerry-Role: agent | client | home-agent | probe | dashboard
```

Relays also accept the former `X-TunnelDesktop-Role` header during the rename transition.

Roles:

- `agent`: work-side idle socket waiting to be paired.
- `client`: home-side data socket for one RDP connection.
- `home-agent`: Windows, macOS, or Android home-agent status presence.
- `probe`: self-test connection.
- `dashboard`: browser status stream.

The dashboard shows work-agent presence, home-side activity, active stream counts, total stream count, and recent remote addresses. Home-side activity is active when either the lightweight home-app presence socket is connected or an RDP stream is currently bridged. It also serves the DeskFerry icon as `/relay/icon.svg` for favicon and header branding.

### Go Relay Web Service

`relay/go/` is a lightweight Go implementation of the same relay contract. It is the active OCI Always Free VM deployment because it runs as one static binary with a much smaller memory footprint than the Python/FastAPI relay.

It exposes the same user-facing paths:

- `GET /relay/` and `GET /relay/<room>` for the live dashboard.
- `GET /relay/health` for health JSON.
- `GET /relay/status` for JSON status.
- `GET /relay/ws` and `GET /relay/<room>/ws` as WebSocket endpoints.

Build the Linux relay binary:

```powershell
.\build\build-go.ps1
```

The build emits:

```text
dist\bin\deskferry-relay-linux-amd64
```

### Python Relay Web Service

`relay/python/` is a FastAPI/ASGI implementation of the same relay contract. It is useful for hosting on Python-capable App Service plans or for local relay testing without the .NET runtime. The active OCI VM deployment uses the Go relay instead.

It exposes the same user-facing paths:

- `GET /relay/` and `GET /relay/<room>` for the live dashboard.
- `GET /relay/health` for health JSON.
- `GET /relay/status` for JSON status.
- `GET /relay/ws` and `GET /relay/<room>/ws` as WebSocket endpoints.

Run it locally:

```powershell
python -m pip install -r relay\python\requirements.txt
python -m uvicorn app:app --app-dir relay\python --host 127.0.0.1 --port 8000
```

Build the deployable Python zip:

```powershell
python -m pip install -r relay\python\requirements-dev.txt
.\build\build-python-relay.ps1
```

The build emits both the source-style zip and an Oracle Linux 9 / Python 3.9 vendored zip for minimal VM deployments.

### Work Agent

`agent.exe` is the work-side Windows component. It is Windows-service-first because RDP must work while the user is logged out.

Default behavior:

- `agent.exe` with no args uses the default relay room URL.
- `-relay-url <url>` selects a named room.
- `-relay-url` can be repeated to add more relay URLs.
- The service keeps a small pool of idle outbound WebSockets per configured relay URL.
- Each installed work agent keeps a persistent local agent identity and tags each idle socket by slot, allowing relays to replace stale idle sockets after reconnects or service restarts.
- When any configured relay pairs a socket, the agent dials `127.0.0.1:3389` and pipes bytes.

Debug and operations:

```powershell
.\agent.exe -console -relay-url https://test-officialwebsite.azurewebsites.net/relay/workdesk
.\agent.exe -console -relay-url https://test-officialwebsite.azurewebsites.net/relay/workdesk -relay-url http://217.142.228.117/relay/workdesk
.\agent.exe -self-test -relay-url https://test-officialwebsite.azurewebsites.net/relay/workdesk
.\agent.exe -status
.\agent.exe -uninstall
```

### Agent Configurator

`deskferry-agent-configurator-windows-amd64.exe` is the native Windows setup and service management GUI.

It:

- Prefers `D:\DeskFerry\Agent` as the install directory when `D:` exists.
- Copies the selected agent binary to `agent.exe`.
- Installs or updates the automatic `DeskFerryAgent` Windows service with the configured ordered relay URL list.
- Protects every configured room with one optional DPAPI-encrypted password and enables an optional WinRM target when a password is present.
- Enables an optional loopback SMB target, registers the selected Windows SMB server alias, and preserves unrelated server aliases.
- Provides add, update, delete, button reorder, and drag reorder controls for relay URLs.
- Configures SCM restart recovery.
- Starts, stops, restarts, uninstalls, refreshes status, opens the install folder, and runs `agent.exe -self-test`.

### Windows Home App

`home-agent/windows/` is the secure home-side Windows path. It provides:

- A polished control panel with an ordered relay room URL list, local RDP address, proxy mode, status tiles, room details, and activity log.
- A notification-area icon with open, connect, stop, Remote Desktop, and quit actions.
- Windows Credential Manager integration for one shared RDP and WinRM login per destination.
- Persistent home-app presence on the relay dashboard.
- Named work-destination profiles, each with its own primary/fallback relay URL list for presence, status, and RDP stream connections.
- Destination add, rename, delete, and selection controls, plus relay URL add, update, delete, button reorder, and drag reorder controls.
- A loopback RDP listener, normally `127.0.0.1:3390`.
- A loopback WinRM listener, normally `127.0.0.1:3391`, plus an integrated PowerShell command panel.
- Automatic Remote Desktop launch when the user clicks `Connect`.

The Windows package also includes:

- `DeskFerryHomeSetup.exe`, a self-contained GUI/CLI installer and configurator with file access selected by default.
- `DeskFerryHomeNetwork.exe`, an automatic LocalSystem service that owns the virtual adapter and the TCP/445-only relay bridge.
- Wintun 0.14.1 and tun2socks 2.6.0, downloaded by the build with pinned SHA-256 hashes and distributed with their licenses.

### macOS Home Agent

`home-agent/macos/` is the macOS home-side command-line agent. It provides:

- A foreground local RDP listener, normally `127.0.0.1:3389`.
- One outbound `client` WebSocket per local RDP connection.
- A persistent `home-agent` presence WebSocket while it runs.
- Primary/fallback relay URL lists for presence, status, and RDP stream connections.
- `-status` for relay room status.
- `-open-rdp` to write and open a local `.rdp` profile with the configured loopback target.

### Android Home App

`home-agent/android/` is the Android home endpoint. It is not an RDP client by itself; it provides the loopback tunnel that an Android RDP client uses.

It provides:

- A native Android control panel with inline-editable relay URL rows, local RDP port, status tiles, activity log, copy, dashboard, and RDP launch actions.
- A foreground service so the tunnel can keep running while another app is active.
- A loopback RDP listener, normally `127.0.0.1:3389`.
- One outbound `client` WebSocket per local RDP connection.
- A persistent `home-agent` presence WebSocket while the service is running.
- A persistent `dashboard` WebSocket for real-time work-agent and stream status.
- Named work-destination profiles, each with its own primary/fallback relay URL list for presence, status, and RDP stream connections.
- Destination add, rename, delete, and selection controls, plus relay URL add, inline edit, delete, button reorder, and drag reorder controls.

Good free Android RDP client options include Microsoft's Remote Desktop/Windows App client and the open-source FreeRDP-based aFreeRDP client. Configure the RDP client to connect to the DeskFerry local target shown in the Android app.

## Security Model

- Work and home endpoints make outbound WebSocket connections only; use HTTPS/WSS whenever the relay supports it.
- The room name is the pairing key for an unprotected room. Protected rooms additionally require the room-scoped password proof set by the work agent.
- Room passwords are not placed in URLs, service command lines, relay logs, or relay status. The work configurator stores the password as a machine-scope DPAPI blob; home profiles store only the derived proof.
- A room proof is a bearer credential. Use a strong, unique room password and prefer `https://` relays. The plain-HTTP OCI fallback cannot protect a captured proof from interception and replay.
- The relay never dials the work PC or home PC.
- The work agent only dials its configured RDP, WinRM, or SMB loopback target after a relay has paired an authenticated, same-room, same-service home connection.
- WinRM is disabled unless the work configurator has both a room password and a WinRM target. Windows login credentials are supplied by the home user for each command and are not handled by the relay.
- SMB is disabled unless the work configurator has both a room password and an SMB target. The Home SOCKS bridge rejects every destination except the configured synthetic work address on TCP port 445; it is not a general-purpose VPN or proxy.
- SMB authentication and authorization remain Windows responsibilities. DeskFerry neither stores file-share passwords nor bypasses share or NTFS permissions.
- Home apps listen on loopback by default, so other LAN devices cannot connect to local RDP or WinRM listeners unless the user intentionally changes a listen address.

Choose room names that are not obvious. For meaningful access control, also configure a strong room password and use TLS.

This software can route around corporate egress controls to expose an internal RDP session. Confirm that use is permitted by workplace policy. This project intentionally does not add anti-monitoring, stealth, or obfuscation behavior.

## Build Prerequisites

Required:

- Go 1.25+.
- .NET SDK 8+ for the Azure relay.
- Python 3.11+ for the Python relay.
- JDK 17+ plus Android SDK platform 35 and build-tools 35.0.0 for the Android home app.
- Gradle 9.x, or a compatible Gradle installation on `PATH`, for the Android home app.
- `rsrc` for Windows GUI manifest resources; `build\build-go.ps1` installs it under `D:\Go\bin` when missing.

The Windows Home installer build downloads [Wintun 0.14.1](https://www.wintun.net/) and [tun2socks 2.6.0](https://github.com/xjasonlyu/tun2socks/releases/tag/v2.6.0). Both archives and the tun2socks license are verified against pinned SHA-256 values before packaging.

This repo has been built with Go installed under `D:\Scoop` and .NET SDK 9.x publishing the relay as `net8.0`.

## Build Commands

Build Azure relay zip:

```powershell
.\build\build-azure-relay.ps1
```

Build Python relay zip:

```powershell
.\build\build-python-relay.ps1
```

Build Go binaries:

```powershell
.\build\build-go.ps1
```

Build the self-contained Windows Home setup (this also builds Go artifacts unless `-SkipGoBuild` is used):

```powershell
.\build\build-windows-home-installer.ps1
```

On development PCs where SEP flags optimized unsigned Go PE files, retain symbols and disable optimization for the Windows artifacts:

```powershell
.\build\build-windows-home-installer.ps1 -DebugWindows
```

To produce separate unoptimized Windows diagnostic builds, first run `build-go.ps1` so the Windows manifest resources are generated, then run:

```powershell
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
$env:CGO_ENABLED = '0'
go build -gcflags 'all=-N -l' -o dist\bin\deskferry-home-windows-amd64-debug.exe ./home-agent/windows
go build -gcflags 'all=-N -l' -o dist\bin\deskferry-agent-windows-amd64-debug.exe ./work-agent/windows/service
```

These debug binaries are larger and slower and remain unsigned. They are useful for diagnostics or for testing an endpoint-protection false positive, but code signing and an administrator-approved allowlist are preferred for normal deployment.

Build Android home APK:

```powershell
.\build\build-android-home.ps1
```

Artifacts:

```text
dist\azure-relay\deskferry-azure-relay.zip
dist\python-relay\deskferry-python-relay.zip
dist\python-relay\deskferry-python-relay-linux-cp39-vendored.zip
dist\bin\deskferry-relay-linux-amd64
dist\bin\deskferry-agent-windows-amd64.exe
dist\bin\deskferry-agent-configurator-windows-amd64.exe
dist\bin\deskferry-home-windows-amd64.exe
dist\bin\deskferry-home-network-windows-amd64.exe
dist\bin\deskferry-home-setup-windows-amd64.exe
dist\windows-home-installer\DeskFerryHomeSetup.exe
dist\bin\deskferry-home-macos-arm64
dist\bin\deskferry-home-macos-amd64
dist\android\deskferry-home-android-debug.apk
```

The optional unoptimized commands above additionally produce:

```text
dist\bin\deskferry-home-windows-amd64-debug.exe
dist\bin\deskferry-agent-windows-amd64-debug.exe
```

## URL Configuration

Use one shared room name:

```text
https://test-officialwebsite.azurewebsites.net/relay/<room>
```

OCI Go relay example:

```text
http://217.142.228.117/relay/<room>
```

Rules:

- `<room>` is created automatically on first use.
- Reusing the same URL joins the same room.
- The work agent may use multiple relay URLs at once when each URL uses the same `<room>`.
- Home apps may use multiple relay URLs as an ordered primary/fallback list when each URL uses the same `<room>`; graphical apps treat the first row as primary and later rows as fallbacks.
- Relays accept work-agent identity headers and keep only one waiting socket per agent instance and slot, preventing reconnects from inflating idle work-socket counts.
- The WebSocket endpoint is derived automatically as `/relay/<room>/ws`.
- The base `/relay/` path is an overview dashboard.
- No generated pairing files are required for the normal Azure WebSocket path.

## Troubleshooting

### Diagnostic Logs

DeskFerry records connection lifecycle details intended to make intermittent disconnects traceable across the home agent, work agent, and relay. Entries include relay selection and dialing, proxy use, WebSocket connection and close information, pairing identifiers, stream direction, byte and message counts, elapsed time, socket state, cancellation state, and errors. Credentials, proxy passwords, and RDP payload contents are not intentionally logged.

Go-based Windows and macOS agents use daily files named `home-agent-YYYY-MM-DD.log` or `work-agent-YYYY-MM-DD.log`. Android uses `home-agent-YYYY-MM-DD.log`. A daily file rotates to `.old` when it reaches 8 MiB. Expired daily and legacy log files are pruned at startup and on date rollover. The configured retention includes the current calendar day, so the default of 7 keeps today plus the previous six days.

Default locations:

```text
Windows home:  %APPDATA%\DeskFerry\home-agent-YYYY-MM-DD.log
Windows work:  %ProgramData%\DeskFerry\work-agent-YYYY-MM-DD.log
macOS home:    ~/Library/Application Support/DeskFerry/home-agent-YYYY-MM-DD.log
Android home:  <app-specific files>/logs/home-agent-YYYY-MM-DD.log
```

Windows and macOS accept `-log-retention-days <days>`. Android exposes the equivalent setting in its control panel. All three home implementations default to seven days, as does the Windows work agent.

Relay diagnostics are written to standard output and retained by the hosting platform:

- The Azure relay installs UTC, single-line console logging and also writes directly to `%HOME%\LogFiles\Application\deskferry-relay-<instance>-<pid>.log`. Direct file logging remains available when the App Service ANCM stdout capture file is empty. App Service application logging should remain enabled at `Information` level. The production service uses seven-day filesystem HTTP-log retention plus filesystem application-log size and file-count limits. Exact application-log retention by days requires Azure Blob Storage or another Azure Monitor destination because App Service filesystem application logs do not expose a day-retention property.
- The OCI systemd host stores Go relay output in persistent journald. Its deployed journal policy uses `MaxRetentionSec=7day` together with a disk-usage cap. Inspect it with `journalctl -u deskferry-relay.service --since '7 days ago'`.
- Python relay deployments also log connection, pairing, bridge, and disconnect lifecycle events through their ASGI server output; configure retention in the process supervisor or hosting platform.

When investigating a disconnect, collect the home-agent and work-agent entries covering the same timestamp, then correlate them with Azure App Service logs or `journalctl` on the relay that was selected. Pair identifiers and close details distinguish a relay-side termination from a home-to-relay, work-to-relay, or local-RDP failure.

### Repeated RDP Disconnects After A Network Drop

Some corporate proxies expire outbound WebSockets on a fixed interval even while the local RDP session remains active. A reconnect or `replaced waiting agent` message roughly every 15 minutes points to that network path rather than an RDP authentication failure.

For resumable pairs, the relay must release the interrupted bridge before it waits for replacement `resume` sockets. Current Azure and Go relays abort obsolete transports immediately and bound graceful WebSocket closes so a missing close handshake cannot hold the resume path for several minutes. Relay logs include `resume rejected`, `resume attachment waiting`, and `resume attachment released` events for correlation with the agent logs.

After a relay change, verify the deployed resume behavior with:

```powershell
$env:DESKFERRY_COMPAT_RELAY_URL='https://test-officialwebsite.azurewebsites.net'
$env:DESKFERRY_COMPAT_PROXY='direct'
go test .\internal\tunnel -run TestExternalRelayResumption -count=1 -v -timeout 60s
```

The test deliberately drops the relay transport and succeeds only if both sides reattach and continue the same logical stream. Deploying or restarting a relay still interrupts active sessions because resumable state is in memory.

Agents cache successful relay DNS answers for the lifetime of the process. If a later DNS lookup fails transiently, reconnect attempts try the cached addresses while preserving the relay hostname for TLS validation. Session-end log entries include `end_initiator=local_rdp`, `end_initiator=relay`, or `end_initiator=both` to distinguish a local RDP socket close from a relay-side interruption.

### Agent Self-Test Fails Through Proxy

Run:

```powershell
.\agent.exe -self-test -relay-url https://test-officialwebsite.azurewebsites.net/relay/workdesk
```

Self-test checks local RDP and then opens a `probe` WebSocket to each configured relay URL.

Common causes:

- Corporate proxy blocks `CONNECT test-officialwebsite.azurewebsites.net:443`.
- Corporate proxy strips WebSocket upgrades unless the relay connection is tunneled with `CONNECT`.
- Proxy requires authentication not supported by the service account.
- Azure App Service WebSockets are disabled.
- The Azure relay has not been deployed or is not running.
- Local RDP is disabled or not listening on `127.0.0.1:3389`.

### Endpoint Protection Flags Windows Binaries

DeskFerry's locally built Windows executables are unsigned. Some heuristic endpoint-protection products may quarantine a newly built executable or replace it with a zero-byte hidden file. Do not disable or bypass endpoint protection. Prefer these remedies in order:

1. Sign release binaries with an organization-approved code-signing certificate.
2. Have an administrator allowlist the reviewed artifact hash or installation path.
3. Submit the artifact to the security vendor as a false positive.
4. Build the unoptimized diagnostic binaries described under [Build Commands](#build-commands) to determine whether the verdict is specific to compiler layout.

Stop the Windows home app before replacing its executable and verify the new artifact hash before installation. Do not retain renamed copies of old executables after a successful upgrade. The work agent's `-update-service` command uses `agent.exe.previous` only as a transactional rollback file and removes it after the upgraded service reaches the running state:

```powershell
Copy-Item ".\dist\bin\deskferry-home-windows-amd64-debug.exe" "$env:LOCALAPPDATA\Programs\DeskFerry\DeskFerryHome.exe" -Force
.\dist\bin\deskferry-agent-windows-amd64-debug.exe -update-service D:\DeskFerry\Agent\agent.exe
```

Verify the installed files with `Get-FileHash` after the endpoint-protection scan completes. A work-service update briefly interrupts active sessions; subsequent sessions use resumption only when the home agent, work agent, and selected relay all support it.

### Home App Connects But RDP Fails

Check:

- The home app status tiles show a room URL that is also configured on the work agent.
- OCI room URLs use `http://217.142.228.117/...`, not `https://217.142.228.117/...`.
- The room dashboard shows waiting work-agent sockets.
- The agent service is running.
- Work PC allows RDP.
- The configured Windows account is allowed to log in remotely.
- The home app local listen port is not already in use. If `127.0.0.1:3389` fails on the home PC, use `127.0.0.1:3390`.

### Work File Access Fails

Check:

- The work configurator has SMB target `127.0.0.1:445`, the alias used by Home setup, and a room password.
- Home setup used the same room name and password and its file-access checkbox was selected.
- The `DeskFerryHomeNetwork` service is running and the `DeskFerry` adapter has address `198.18.0.1`.
- `Test-NetConnection deskferry-work -Port 445` reaches `198.18.0.2` on the Home PC.
- The named Windows share exists and the supplied Windows account has both share and NTFS permission.
- The work PC was restarted once after adding or changing the SMB server alias.

If Windows reports that the target account name is incorrect in a domain, the alias lacks a matching Kerberos SPN. Register the alias through the domain's normal computer-alias/SPN administration, use the actual work hostname as the configured alias, or permit the organization's approved NTLM fallback. Microsoft documents the alias/SPN behavior in [SMB file server share access through an alias](https://learn.microsoft.com/en-us/troubleshoot/windows-server/networking/dns-cname-alias-cannot-access-smb-file-server-share).

### OCI Relay Becomes Unresponsive

The OCI Go relay is supervised by `deskferry-relay.service`, a local health timer, and systemd's five-minute runtime watchdog:

```bash
systemctl status deskferry-relay.service
systemctl status deskferry-relay-healthcheck.timer
journalctl -u deskferry-relay-healthcheck.service -n 50
systemctl show --property=RuntimeWatchdogUSec --property=RebootWatchdogUSec
free -h
swapon --show
```

The health timer restarts the relay when the local `/relay/health` endpoint fails and reboots after repeated local failures. The systemd watchdog can recover some userland or kernel stalls. If SSH and HTTP both hang while OCI still shows the VM as running, the hypervisor-side virtual network path may be wedged; use an OCI instance reset, OCI-side monitoring with a recovery action, or a larger/more reliable relay host for that failure mode.

### Saved Windows Login

The Windows home app can save one shared RDP and WinRM login per destination through Windows Credential Manager. Enter the work Windows username and password, then click **Save Windows Login**. DeskFerry stores the destination credential for WinRM, calls `cmdkey.exe` for the local RDP target, writes a password-free `%APPDATA%\DeskFerry\home-client.rdp` launch profile, and clears the password field after saving. The password is not written to `%APPDATA%\DeskFerry\home-client.json` or the `.rdp` profile. **Forget Windows Login** removes both saved uses of the credential.

**Open Remote Desktop** and **Connect** launch MSTSC with that `.rdp` profile, so saved credentials are used automatically when Windows allows them. **Execute** in the WinRM Commands panel uses the same destination credential. Remote Desktop may still prompt if Windows policy blocks saved credential delegation. In that case, allow saved credentials for the `TERMSRV/*` target through Windows policy, or continue signing in manually.

### Azure Relay Status

Open:

```text
https://test-officialwebsite.azurewebsites.net/relay/workdesk
```

The dashboard receives live status over WebSocket. Useful fields:

- waiting work-agent sockets
- home-app presence
- active RDP stream pairs
- total stream pairs
- last home-client source address

## Development

Run all Go tests:

```powershell
go test ./...
```

Build and test the Azure relay:

```powershell
.\build\build-azure-relay.ps1
```

Rebuild after Go agent/client/relay changes:

```powershell
.\build\build-go.ps1
```

## Repository Layout

```text
relay/azure-dotnet/      .NET Azure App Service WebSocket relay
relay/go/                Go WebSocket relay used by the OCI VM deployment
relay/python/            Python/FastAPI WebSocket relay
work-agent/windows/service
                         Windows service work-side agent
work-agent/windows/configurator
                         Windows service setup/configurator GUI and CLI
home-agent/windows       Windows control-panel/tray home app
home-agent/windows/installer
                         Self-contained Windows Home setup/configurator GUI and CLI
home-agent/windows/network-service
                         Restricted Wintun/tun2socks SMB network service
home-agent/macos         macOS foreground CLI home agent
home-agent/android       Android foreground-service home app
internal/tunnel          WebSocket, proxy, pipe, and role helpers
build/                   build scripts
```

## Status

This repo currently contains:

- Azure App Service WebSocket relay source and publish script.
- Lightweight Go WebSocket relay source and Linux build artifact for OCI.
- Protocol-compatible Python WebSocket relay source and publish script.
- Live dashboard with WebSocket status updates.
- Named-room URL joining under `/relay/<room>`.
- Windows work agent implemented as a Windows service deliverable.
- Windows configurator GUI for installing and managing the work agent service.
- Windows home app implemented as a friendly control-panel and tray deliverable.
- Optional Windows Home virtual network adapter and self-contained installer for `\\deskferry-work\<share>` access.
- macOS home agent implemented as a foreground CLI tunnel endpoint.
- Android home app implemented as a foreground-service loopback tunnel endpoint.
- Build scripts for Go binaries, the Windows Home installer, relay packages, and the Android APK.

## Current Limitations

- The Azure relay is a simple in-memory broker. Restarting the App Service disconnects active sessions and clears room status.
- Multiple App Service instances are not supported unless sticky routing or shared broker state is added.
- The Go work and home agents support direct, environment, HTTP proxy, and HTTPS proxy modes. Proxy URLs may contain Basic credentials; NTLM proxy authentication is not implemented.
- The Windows home app is an RDP launcher and tunnel endpoint, not a full RDP client.
- Windows UNC access currently transports SMB only. It does not carry arbitrary IP traffic, SMB discovery, or printer discovery; users open a known `\\alias\share` path.
- The macOS home agent is a tunnel endpoint and `.rdp` launcher, not a full RDP client; use Microsoft Remote Desktop/Windows App or another macOS RDP client against `127.0.0.1:3389`.
- The Android home app is also a tunnel endpoint, not a full RDP client; use a separate Android RDP client against `127.0.0.1:3389`.
