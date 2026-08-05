# Release Notes

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
