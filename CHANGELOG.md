# Release Notes

## Unreleased

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
