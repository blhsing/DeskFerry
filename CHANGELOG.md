# Release Notes

## 0.10.11 - 2026-08-13

- Fix passwordless local-account RDP authentication by resolving a bare username such as `Owner` through WinRM and saving it as the Work computer-qualified identity, such as `DESKTOP-G2EEM1V\Owner`.
- Configure the Work computer and generated RDP profile to use classic RDP security for an explicitly passwordless profile, because CredSSP/NLA rejects blank passwords even when Windows permits other blank-password network logons.
- Remove stale domain-password entries before saving loopback RDP credentials, preserve explicitly qualified domain, computer, and UPN usernames, and explain when a passwordless profile has no saved room credential.

## 0.10.10 - 2026-08-12

- Keep resumable RDP sessions alive when a proxy or hosting layer reports a transport interruption as WebSocket close code 1000 without DeskFerry's explicit `session closed` marker.
- Make the shared Work, Windows Home, and macOS Home transport automatically resume after the same normal-code interruption.
- Apply the logical-close distinction consistently to the Azure .NET, OCI Go, and Python relays, and make Azure immediately wake both endpoints by aborting obsolete transports.
- Make the Android Home agent automatically resume an active RDP or SMB stream after the same normal-code transport interruption, while reserving `session closed` for intentional local shutdown.

## 0.10.9 - 2026-08-11

- Retry transient Windows desktop-capture failures caused by RDP attach/detach, unlock, display-mode, and input-desktop transitions before the Work agent returns an error frame.
- Make the Android Screen Viewer automatically rebuild a failed paired screen session with bounded retries, reset partial frame state between attempts, and retain the final Work-side error when recovery is exhausted.

## 0.10.8 - 2026-08-10

- Raise the Work agent's default live-session concurrency limit from 4 to 32 so WinRM can open its supporting connections while RDP, SMB, and screen sessions are active.
- Retain the bounded `DESKFERRY_MAX_SESSIONS` override, which accepts values from 1 through 256.

## 0.10.7 - 2026-08-10

- Allow Windows and macOS Home agents to save and use Windows logins with intentionally blank passwords for WinRM when the Work computer permits blank-password network logons.
- Add an explicit macOS Save Windows Login action so a blank password is distinguishable from keeping an existing Keychain credential.

## 0.10.6 - 2026-08-08

- Changed Work screen sharing to capture the primary display instead of composing the complete virtual desktop, eliminating black secondary-display regions on Windows lock screens for Windows, macOS, and Android Home viewers.
- Fixed the Windows Screen Viewer layout so its toolbar remains compact, Auto Fit receives the full viewport, and large remote screens open maximized while low-resolution screens use a fitted window.
- Kept focus on the Windows Screen Viewer after it opens and after its first captured frame determines the final window size.

## 0.10.5 - 2026-08-08

- Made authenticated Work screen capture recover a logged-on but disconnected Windows session by reattaching it to the physical console, waiting for the interactive desktop, and launching the helper with a fresh user token.
- Added Windows and macOS Home CLI commands for one-shot screenshots and reconstructed screenshot streams using saved destinations, without opening either graphical control panel.
- Fixed Android RDP recovery after mobile/Wi-Fi disruption by bounding each resume handshake and waking the resume worker immediately when Android replaces an obsolete network transport.
- Added Android resume timing tests to the normal APK build and made the Android version prominently visible in the control-panel title.

## 0.10.4 - 2026-08-07

- Fixed direct Work screen capture on RDP and indirect-display desktops by probing every attached DXGI adapter/output and creating each capture device on the adapter that owns the output, including software-backed remote display adapters.
- Fixed `-update-service` precedence so a one-shot LocalSystem service can transactionally upgrade a saturated remote Work agent without being mistaken for the normal agent service.
- Verified Room b end to end through the authenticated DeskFerry `screen` protocol after deployment, with no RDP pixel transport or retained rollback binary.

## 0.10.3 - 2026-08-07

- Fixed Windows Work screen capture on RDP and display drivers that reject GDI desktop copies by binding the capture thread to the active input desktop and falling back to DirectX Desktop Duplication.
- Added automatic DirectX capture reinitialization after RDP reconnects, display-mode changes, lock/unlock transitions, and other desktop switches.
- Expanded Work-side capture errors with the attempted desktop dimensions and Windows failure details so relay-queued logs can distinguish GDI, desktop-binding, and DirectX failures.

## 0.10.2 - 2026-08-07

- Fixed Windows Work screen capture by using a top-down DIB section and retrying without `CAPTUREBLT` for RDP display drivers that reject layered-window capture.
- Kept screen relay sessions open until Home consumes the helper's final image or error frame, preventing a successful helper write from surfacing as a frame-header EOF.
- Added Auto Fit, preset and arbitrary zoom levels, and drag-to-pan to every Home screen viewer; Windows and macOS also support wheel/trackpad zoom, while Android supports native pinch-to-zoom.

## 0.10.1 - 2026-08-07

- Fixed Work screen capture when the interactive user is in an RDP or other active terminal-services session: the service now enumerates active sessions instead of assuming the physical console owns a user token.
- Added Windows and macOS Home command-line WinRM execution with saved destination selection, saved Windows credentials, self-contained temporary relay tunnels, command-file/stdin input, and configurable timeout.
- Added matching macOS Keychain-backed Windows-login fields and WinRM command controls, plus a Windows Home CLI maintenance action for resynchronizing the selected profile to the optional SMB network service.
- Updated agent WebSocket identification, startup records, control surfaces, relay dashboards, health responses, installers, and Android metadata to version 0.10.1.

## 0.10.0 - 2026-08-07

- Added an authenticated, opt-in Work screen service that captures the interactive Windows desktop without opening RDP.
- Added screenshot and periodic-stream viewers to the Windows, macOS, and Android Home agents, with popup/viewer windows, fullscreen display, and PNG saving.
- Made streaming bandwidth-efficient: the Work agent sends one initial PNG and then only changed 64-by-64 image tiles, with empty heartbeats when the screen is unchanged.
- Added a profile-oriented macOS Home control panel with the same relay-base, room, connection, status, and screen-view workflows as the Windows Home app while preserving `-ui=false` command-line operation.
- Added an Android loopback SMB forward on configurable unprivileged port `1445` by default, allowing CX File Explorer to reach Work SMB shares without rooting the phone.
- Extended the Go, Azure .NET, and Python relays with strict `screen` service routing and service confirmation, preventing older relays from silently treating screen requests as RDP.
- Bumped the Android package to version code 1000 under the stable release signing path so it upgrades 0.9.4 and newer installations in place; the SemVer code scheme (`major * 10000 + minor * 100 + patch`) reserves code 10000 for a future 1.0.0 without collision.
- Displayed version 0.10.0 in every Work/Home control surface and recorded it explicitly at each Work/Home agent startup for device and relay-side diagnostics.
- Displayed version 0.10.0 on the Go, Azure .NET, and Python relay web dashboards and exposed it through each relay health response.

## 0.9.5 - 2026-08-06

- Made Android react immediately to mobile/Wi-Fi network loss and handoff by replacing active relay transports instead of waiting for a stale WebSocket failure.
- Reduced Android WebSocket liveness detection to ten seconds and limited concurrent local RDP bridges to two, preventing reconnect probe storms from exhausting Work-agent concurrency before the real desktop reconnect arrives.
- Fixed Android 6 compatibility checks found while validating the recovery build, and made the Android project pass its full debug lint task.

## 0.9.4 - 2026-08-06

- Split Home destination and Work agent pairing settings into one room name plus an ordered list of relay service base URLs.
- Pre-filled new profiles with the Azure primary and OCI fallback relay services while migrating existing full room URLs automatically.
- Added stable repository release signing for Android APKs so future releases can upgrade in place. Because releases through 0.9.3 used unrecoverable ephemeral CI debug keys, upgrading to 0.9.4 requires one final uninstall of the old app.

## 0.9.3 - 2026-08-06

- Added authenticated, acknowledged diagnostic-log uploads from the Windows Work agent and Windows, macOS, and Android Home agents to every configured relay.
- Queued logs generated before relay connectivity in bounded process-local backlogs while preserving the existing retained on-device diagnostic files.
- Added consistent `agent_log` correlation fields across the Azure .NET, OCI Go, and Python relays, including room, component, device instance, and remote address.
- Preserved active Windows Home destination isolation by switching diagnostic upload targets when the selected destination changes.

## 0.9.2 - 2026-08-06

- Made Android close stale local RDP sessions immediately when relay resumption receives a terminal normal-close, unknown-session, or authentication rejection.
- Stopped newly accepted Android RDP sockets from retrying busy or unavailable relay rooms for five minutes, allowing Microsoft Windows App reconnect attempts to fail promptly instead of remaining on **Establishing connection**.

## 0.9.1 - 2026-08-05

- Stopped new local RDP negotiation or retry sockets from forcibly closing an established desktop session in the Windows and macOS Home agents.
- Tracked concurrent local RDP sockets independently while still closing all of them cleanly when a tunnel stops.

## 0.9.0 - 2026-08-05

- Reused one authenticated PowerShell Remoting session for WinRM commands in the Windows Home app, with five-minute idle expiration and teardown on destination, credential, tunnel, and app changes.
- Added per-command elapsed-time and session-reuse diagnostics plus an opt-in live WinRM benchmark.
- Avoided automatically replaying commands after ambiguous session failures, preventing duplicate remote side effects.
- Made the SMB alias a per-destination Home profile setting editable directly in the Home UI, and added elevated selected-profile synchronization for the restricted adapter service and managed hosts entry.
- Removed the redundant Home **Refresh** button because relay and presence status already update automatically.
- Suppressed transient console windows from background credential, WinRM, and elevated network-configuration helpers launched by the Windows Home GUI.

## 0.8.0 - 2026-08-05

- Replaced fixed work-agent data slots with one protocol-v2 control connection per relay and on-demand RDP, WinRM, and SMB session sockets.
- Added bounded pending offers, configurable live-session concurrency, and immediate typed `busy`, `no-agent`, authentication, service, version, and timeout results across the Azure .NET, OCI Go, and Python relays.
- Made Windows, macOS, Android, and the Windows Home network service log every relay attempt and select fallback relays promptly while treating authentication and configuration failures as terminal.
- Made normal completion, unknown-session rejection, and authentication rejection terminal so completed sessions no longer create resume storms or temporary slot starvation.
- Preserved the legacy slot protocol behind dual-protocol relays and the `DESKFERRY_FORCE_LEGACY=1` work-service rollback switch.
- Expanded relay status with control identities, protocol version, active and pending sessions by service, and busy/no-agent rejection counters.
- Made in-place Windows Home upgrades preserve the selected destination's saved room password proof unless a new password is explicitly supplied.

## 0.7.2 - 2026-08-05

- Made the Windows Home app register the selected destination's saved Windows login for the installed SMB alias as well as RDP and WinRM.
- Refresh the alias credential automatically at startup and when a destination is selected or connected, allowing Explorer to authenticate without contacting the work domain controller from the Home PC.
- Remove the alias credential together with the destination login when **Forget Windows Login** is used.
- Read customized SMB aliases from Home installer metadata and keep passwords exclusively in Windows Credential Manager.

## 0.7.1 - 2026-08-05

- Fixed Windows PowerShell stdin adding one or more UTF-8 byte-order marks to room passwords supplied through the command-line configurators.
- Added regression coverage for BOM-prefixed password input in both the Work Agent Configurator and Home installer.
- Prevented apparently successful protected-room setup from storing a credential that Home agents cannot authenticate with.

## 0.7.0 - 2026-08-04

- Added optional room passwords shared by RDP, WinRM, and SMB connections.
- Added WinRM command execution from the Windows Home app using the same saved Windows credentials and destination profiles as RDP.
- Added SMB/UNC access from Windows Home PCs through an optional Wintun virtual adapter. The adapter exposes only the configured synthetic work-host address on TCP port 445; it is not a general-purpose VPN.
- Added the self-contained `DeskFerryHomeSetup.exe` installer. The virtual network adapter component is optional and selected by default.
- Added work-agent configuration for the SMB target and friendly `deskferry-work` server alias, enabling paths such as `\\deskferry-work\sharename`.
- Added SMB service isolation and password enforcement to the Go, Azure .NET, and Python relay implementations.
- Refined the Windows Home interface to fit a 1366 x 768 desktop while keeping connection, WinRM, room details, and activity controls visible.
- Documented OCI deployment through the required HTTP CONNECT proxy and expanded build, installation, security, and troubleshooting guidance.

## 0.6.0 - 2026-07-28

- Added password-protected rooms and WinRM relay support.
- Added shared Windows credential storage for RDP and WinRM.
- Refined Windows Home destination profiles, relay URL management, and compact desktop layout.
