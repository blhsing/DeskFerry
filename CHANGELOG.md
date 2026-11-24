# Release Notes

## 0.15.8 - 2026-11-24

- Fix the Windows Screen Viewer's pointer turning into a black box over anything that is not black while you have control. The viewer builds a local pointer from the Work PC's pointer shape but gave it an empty mask, so whenever Windows drew it without its transparency every transparent pixel came out black. The pointer now has a real mask and stays transparent around its shape.
- Capture the Work PC's pointer by drawing it the way Windows does, over black and over white, and deriving its colors and transparency from the two, instead of interpreting the pointer's bitmaps. This handles pointers with alpha, masked color and monochrome pointers, including inverting ones such as the text cursor, the same way.

## 0.15.7 - 2026-11-23

- Reach video quickly when a busy screen is opened. Since 0.15.6 the first picture was lossless, which for a 2880x1800 screen saver is several megabytes and kept anything else from arriving for seconds; every compression step tried meanwhile measured 0 frames a second, was judged useless and put on cooldown, so video could take more than a minute to win. The first picture now sends photographic areas as JPEG and refines them losslessly right after (Min fps 0 stays lossless throughout), and a trial during which nothing arrives at either level keeps measuring instead of failing.
- Stop the regular frame-rate dips while video is running. With frame rate above Min fps, the controller used to try JPEG every half minute, judged the trial from frames that were still arriving as video, kept JPEG, and fell to a few frames a second until it returned to video. Lighter levels are now judged only after the stronger level's frames have drained, and from video they are tried only once the moving area has shrunk.

## 0.15.6 - 2026-11-23

- Keep the picture lossless while the frame rate is above Min fps. At full quality the Work PC used to send photographic-looking areas as JPEG whenever they changed, even with plenty of frame rate and bandwidth to spare; full quality is now lossless (PNG-style), and JPEG, then video, are used only when fewer frames than Min fps get through.
- Re-send lossy areas losslessly as soon as each one stops changing. Refinement used to wait until the whole screen had been still for a quarter of a second, so a ticking clock, a spinner or a screen watermark anywhere on screen could keep the rest of the picture lossy indefinitely. Refinement and new changes now take turns, and a video stream whose area has gone still ends so its area can be refined.

## 0.15.5 - 2026-11-23

- Stream large moving areas, such as video or a screen saver, as H.264 video when that keeps the frame rate up. When fewer frames than the viewer's Min fps get through and lower JPEG quality is not enough, the Work PC sends the region that keeps changing as a hardware-encoded H.264 stream (Intel Quick Sync, AMD or NVIDIA through Windows Media Foundation, or Microsoft's software encoder), at a bitrate below the throughput the viewer actually acknowledges, and keeps it only if the frame rate rises; once the area stops changing it is re-sent losslessly as before, and Min fps 0 never uses video. The Windows viewer decodes it with Windows Media Foundation, the macOS viewer with the browser's WebCodecs, and the Android viewer with MediaCodec; older viewers keep receiving tiles. On a simulated 1.5 MB/s link with a 1280x720 area in constant motion, the frame rate rose from about 25 frames a second with JPEG to about 40 with video.
- Keep compression steady while the screen keeps moving. The controller used to treat a screen as calm once compression kept up with it (nothing was waiting any more) and dropped back to full quality, then climbed through the JPEG steps again; it now judges calm from whether the screen is still changing, returns straight to the step that worked when motion resumes, goes straight to video when most of the screen moves, and tries less compression only after five seconds of spare frame rate or once the moving area has shrunk well below what the current step was kept for. A video stream now covers the whole screen when most of it moves and widens instead of restarting for small changes in the moving area. On a 2880x1800 Work PC running a screen saver through the relay, the frame rate rose from 2 or 3 frames a second to a steady 22 to 25.
- Fix video encoders that failed after a while with "media buffer has no memory" or a crash: values the Windows codec returned could be written to a stale copy of the program's stack. A failed stream is now restarted, and the session falls back to tiles only after three failures.
- `-update-service` no longer reports a failure when the update succeeded but the previous executable is still in use by the desktop app; that copy is now deleted at the next restart.
- Track screen motion from what captures actually see, so the Work PC's own pauses (such as starting a video encoder) no longer reset it.
- Speed up the Work PC's per-frame work for large screens: change detection and tile classification now use all processor cores instead of one, and a needless full-frame pass after each capture is gone. Encoding a whole 2880x1800 frame takes about 21 ms instead of about 35 ms on a 12-core PC.
- Send frames from a separate writer, so the Work PC captures and encodes the next frame while the previous one is still being sent, and always allow about three typical frames in flight.
- Stop the compression controller from stalling below the step that would help: when one step does not raise the frame rate, the next attempt tries the step after it, such as reduced resolution, instead of waiting for the failed step to cool down.
- Report the Work PC's frame timings (capture, change detection, encoding, and how often changes wait for the viewer) in the screen status, also shown by agent mode's `screen_status`, to diagnose low frame rates.

## 0.15.4 - 2026-11-23

- Add **Min fps** to the Screen Viewer on Windows, macOS and Android, and `-agent-min-fps` for agent mode: the frame rate below which the Work PC starts compressing changing areas (default 30, remembered per destination). 0 means no lossy compression at all: everything is sent losslessly at full resolution, using only PNG-style lossless compression. A change applies to the running session at once.
- Fix live screen sessions that could stop for good after about 10 seconds. When both sides sent their periodic keep-alive at the same moment, each side's reader could block writing its reply while the other side was blocked writing too, so neither read again; a slow link with full buffers made this more likely. Replies and acknowledgements now go out through a separate writer, so readers never block. 0.15.3 has this bug and is superseded by this release.

## 0.15.3 - 2026-11-23

- Keep the frame rate up when a large area of the screen changes for a long time, such as video or a screen saver. Compression now follows the frame rate that actually reaches the viewer: while changes are waiting and fewer than 30 frames a second arrive, the Work PC tries one step more compression (lower JPEG quality, then moving text areas as JPEG, then moving areas at 1/2, 1/3 or 1/4 resolution, which the viewer scales back up), and keeps the step only if the frame rate measurably rises. With frame rate to spare it steps back toward full quality, and when the screen is still it re-sends everything losslessly at full resolution as before. On a simulated 1.5 MB/s link with the whole 1280x720 screen changing, the frame rate rose from about 4 to about 39 frames a second. Ordinary desktop use, which already reaches 30 frames a second, stays at full quality.
- Show the current compression in the Windows viewer's status bar.
- Reduced-resolution tiles are offered only to viewers that announce them: the Windows, macOS and Android viewers and agent mode from this version. Older viewers keep receiving full-resolution tiles.

## 0.15.2 - 2026-11-23

- Fix the Windows Screen Viewer showing a black screen. Frames arrived, but the viewer drew them with a call the window's paint surface rejected; it now draws them through a GDI bitmap, as before 0.15.0.
- Use the saved room password for live screen sessions on every platform, with no extra prompt. The session key is now derived from the room proof each Home app already saves, so the Windows viewer no longer asks for the room password, destinations saved before 0.15.0 work as they are, and a key typed into 0.15.0's prompt can no longer be lost when settings are saved. Because the relays see the room proof, they could derive this key too; the encryption still keeps the screen private from everyone else. Home apps and Work agents on 0.15.0 or 0.15.1 cannot open live sessions with 0.15.2 until both sides are upgraded.
- Say why a view is read-only: the viewer no longer blames an older Work PC when the destination simply has no room password.

## 0.15.1 - 2026-11-23

- Fix `-agent-mcp` and `-agent-json` through the relays. The agent closed its relay connection as soon as the screen session had opened, so every screenshot and action failed with "no screen image has arrived yet". Tests had used an in-memory connection that did not show this. The viewers were not affected.

## 0.15.0 - 2026-11-23

- Turn the Screen Viewer into a live remote-desktop client. The Work PC now streams its screen continuously at the best frame rate and quality the connection allows: it sends only changed tiles, keeps text lossless, sends photographic areas as JPEG at a quality chosen from the measured bandwidth, re-sends them losslessly as soon as the screen is still, and waits for the viewer's acknowledgements so a slow link lowers the frame rate instead of building a backlog. Desktop Duplication capture is event-driven, and the remote pointer is drawn separately. The Windows, macOS and Android viewers all use it; the interval selector and one-shot capture are gone.
- Add remote control. **Take control** in the Windows viewer (or Ctrl+Alt+Shift+C), the macOS viewer's Control toggle, and the Android viewer's Control button send the mouse and keyboard to the Work PC at any time, shared with the person sitting at it, together with Ctrl+Alt+Del and clipboard exchange. The Work PC owner enables it with the new **Allow remote control** option in Work Services (`-screen-control`); it is off by default. With control on, the Work service allows services to send Ctrl+Alt+Del through the `SoftwareSASGeneration` policy when that policy is not configured.
- Show an always-on-top banner on the Work PC during every screen session, saying who is viewing, whether it is a person or an AI agent, and whether they can control the PC, with **Stop control** and **Stop sharing**. It replaces the console window the capture helper used to open.
- Run the screen helper as LocalSystem in the signed-in user's session where possible, so the lock screen, sign-in screen and UAC prompts can be viewed and controlled. It falls back to the user's identity as before.
- Encrypt live screen sessions end to end between the Home app and the Work PC with a key derived from the room password (P-256 key agreement authenticated by that key, then AES-256-GCM), so the relays cannot read the screen or inject input. Home apps save the new key with the room proof; destinations saved by an older version need the room password once more, which the Windows viewer asks for and checks against the saved proof. The protocol is documented in `docs/screen-live-protocol.md`. Work PCs running an older DeskFerry are shown read-only through the previous screenshot stream.
- Let AI agents read and control a Work PC. `DeskFerry.exe -agent-mcp -destination <name>` (and the macOS binary with the same options) runs a stdio MCP server whose tools follow the computer-use vocabulary (`screenshot`, `zoom`, `left_click`, `type`, `key`, `scroll`, `left_click_drag`, ...) in screenshot-pixel coordinates, wait for the screen to settle after each action and report what changed, and add `ui_tree` and `ui_find` for reading the Windows UI Automation tree, clipboard access and Ctrl+Alt+Del. Agent input pauses while a person uses the Work PC. `-agent-json` offers the same actions as JSON lines for scripts.
- The relays are unchanged; the version number of the Azure and Python relays moved only with the release.

## 0.14.7 - 2026-11-22

- Revert the 0.14.6 change that ended interrupted Remote Desktop sessions when a new one arrived. Closing the old connection to the Remote Desktop server just before the reconnecting client signed in raced with Windows handing the desktop to the new connection, and Windows then disconnected the reconnecting client as if another computer had taken over. The Work service again leaves the old connection open, and the reconnecting client takes over the desktop from it as before.

## 0.14.6 - 2026-11-22

- Reconnect Remote Desktop to the original desktop after an interrupted session gives up. When the Remote Desktop client abandoned an interrupted session and reconnected, the Work service still held the old session's connection to the Remote Desktop server for up to five minutes while it waited to resume. Windows could not hand the reconnecting client its original desktop and started a new logon session instead. The Work service now ends any interrupted Remote Desktop session as soon as a new one arrives, before connecting it to the server; a new connection would displace the old one anyway.

## 0.14.5 - 2026-11-21

- Stop the two sides of an interrupted session from missing each other at the relays. The side that noticed the loss first waited only 3 seconds at the session's relay before moving to the next one, but the other side may notice only when its heartbeat times out (8 seconds for Remote Desktop). The first wait at the session's relay now lasts at least the heartbeat timeout, on Windows and macOS. The Android Home app is unchanged.
- Close the local connection at once when a session cannot resume. A failed stream only half-closed the local connection, and a Remote Desktop server that had not yet received any data kept it open for hours, also holding one of the Work service's session slots.

## 0.14.4 - 2026-11-16

- Stop the optional DeskFerry virtual network adapter from becoming the preferred network interface. Windows gave it an automatic interface metric of 5, ahead of Wi-Fi or Ethernet, so broadcasts and multicast sent without a chosen interface, such as printer, casting, and other LAN discovery, left through the DeskFerry adapter and were dropped. The Home network service now sets the adapter's IPv4 and IPv6 metric to 9999; Work file access is unaffected because the work address is reachable only through the adapter.
- Stop logging an "only SOCKS CONNECT is supported" line for each UDP broadcast or multicast packet Windows sends on the adapter. These UDP flows are still refused, because the bridge carries only TCP port 445.

## 0.14.3 - 2026-11-16

- End a Remote Desktop, WinRM, or other session within seconds when the Work agent accepts it but cannot finish connecting its side to the relay. Through a slow Work-side proxy, the relay could complete the agent's connection while the agent timed out waiting for the answer; the agent then gave up silently, and the relay and Home app kept the session open for the whole five-minute resume window, so each reconnect attempt seemed to hang. The Work agent now reports `session-closed` to the relay when its side fails after accepting, so the Home app sees the failure at once and a new attempt can start.
- Close a resume connection that is still waiting when its session ends with a terminal "unknown resumable session" rejection, in the Azure, Go, and Python relays. The Go relay previously left such a connection open, so the Home app waited on it until its resume window ran out.

## 0.14.2 - 2026-11-16

- Mark a Work agent offline when its host disappears without closing the connection, for example on power loss or a network drop. The Work agent sends a heartbeat every 3 seconds, but the relays never timed out a silent control connection, and the Azure front end kept the half-open socket alive, so the dashboard kept showing the Work agent online and Home apps kept trying to connect to it. The Azure, Go, and Python relays now drop a control connection after 30 seconds without a message, once that agent has sent at least one heartbeat. Agents too old to send heartbeats are unaffected.

## 0.14.1 - 2026-11-15

- Start the Work agent from its Scheduled Task fallback. When the Work service cannot be installed as a Windows service, DeskFerry registers a logon task instead, but the task ran `DeskFerry.exe -console`, which opens the Home app, so the Work agent never started. The task now runs the new `-work-console` mode, which starts the Work agent in the foreground. The task's command line also doubled every backslash in its paths; it now quotes them the Windows way.

## 0.14.0 - 2026-11-14

- Add TCP forwards: reach a host and port on the Work network, such as a database or an SSH server, from a loopback port on the Home PC. The Work PC owner lists the targets Home apps may reach, each under a forward port number, in the new **TCP forwards** card of the Work Services window or with `-tcp-forward forwardPort=host:port` (for example `-tcp-forward 5432=db01:5432`). On the Home PC, the selected destination's new **TCP forwards** card maps a local port on `127.0.0.1` to a Work forward port (for example `15432=5432`); the listeners start with **Connect**. `DeskFerry.exe -console -tcp-forward 15432=5432` does the same for one run. Each forward travels as its own relay service, `tcp-<forward port>`, through the existing relay room, so the relays open no new ports and only the Work PC knows the target. Like WinRM and SMB, TCP forwards require a room password.
- Accept TCP forward services in the Azure, Go, and Python relays. A Home app requires the relay to confirm the requested forward service, so an older relay that would map it to Remote Desktop is skipped instead.

## 0.13.13 - 2026-11-04

- Allow the Work service to read Home settings without blocking atomic replacement, and retry brief Windows sharing locks when saving settings or Windows logins. Persistent errors still leave the previous settings intact.
- Create the DeskFerry Start menu shortcut for Work-only installations as well as Home installations.

## 0.13.12 - 2026-11-04

- Keep the Windows app responsive when Save login cannot reach the Work computer. Saving a login that needs the Work computer (qualifying a local username or changing Remote Desktop security) now checks the Work agent first and gives up within seconds when it is offline, instead of retrying the relay for 90 seconds. Saving a username saves it as entered; a security change reports that the Work computer was not changed. Stopping the tunnel, saving settings, or switching destination while such a command runs now cancels it at once, where the window used to stop responding until the command timed out.

## 0.13.11 - 2026-11-04

- Make mouse clicks work on the Windows app's filled primary buttons, such as Connect / Stop Tunnel in the Home window and the primary actions in the Setup and Work Services windows. The button took mouse capture on press, and the window framework then took it again for the same button, which the button read as a lost press, so it ignored the release. Only Space and Enter activated these buttons. A capture notice for the button itself no longer cancels the press.

## 0.13.10 - 2026-10-24

- Tell a POST/GET streaming client at once when the relay ends its transport. When one side of a session reconnected, the Azure and Go relays aborted the other side's transport to resume both together, but a streaming client only received empty responses until its own heartbeat timed out several seconds later. By then the other side had moved on to the next relay, and the two kept missing each other until the Remote Desktop client gave up. The relays now send a close record on the client's next download, so it resumes within a fraction of a second. The Python relay already did this.
- Wait longer at the room's other relays when the session's relay cannot be reached. A side that cannot reach that relay cannot return to it, so it now waits at least 8 seconds at the other relays (12 seconds per attempt on Android), giving the other side time to give up on the session's relay and follow. Resume diagnostics now name both relays when a session moves.

## 0.13.9 - 2026-10-23

- Move interrupted Android Home sessions to the room's other relays too. When the session's relay cannot be reached, the Android Home app now tries the room's relays in the configured order, as the Windows and macOS apps and the Work service do since 0.13.8. With more than one relay, each attempt allows 4 seconds plus 3 more per round (up to 15) for connecting and for the Work agent to arrive.
- Stop pairing a resume connection that a side has already abandoned. A side that gives up waiting at one relay closes that resume connection and tries the next, but the relay did not notice until it tried to pair it, which cost another full resume round. The Azure, Go, and Python relays now read waiting resume connections continuously, drop one as soon as it closes, and no longer interrupt a working session because of one.

## 0.13.8 - 2026-10-23

- Move an interrupted Remote Desktop, WinRM, or SMB session to another relay of the same room when its relay cannot be reached. A resume previously retried only the relay the session started on, so when one side's path to that relay stayed down longer than the Remote Desktop client's ~16-second reconnect window, the session was lost even though the room's other relay was reachable. The Windows Home app, the Work service, and the macOS Home agent now try the room's relays in the configured order, waiting a bounded and growing time at each for the other side, and every relay rebuilds the session from the two resume attachments. The Android Home app still resumes only through the session's original relay.

## 0.13.7 - 2026-10-19

- Reconnect Remote Desktop sessions sooner after the relay path recovers from a freeze. A resume attempt started while the path was frozen kept waiting for its full 8-second limit after the path came back, and failed attempts also doubled the delay before the next one, so a session could resume a fraction of a second after the Remote Desktop client had already given up. On Windows and macOS, Remote Desktop resume attempts now give up after 4 seconds, returning to 8 seconds after three consecutive timeouts in case a slow proxy needs longer. On every client, an attempt that timed out no longer increases the delay before the next one.

## 0.13.6 - 2026-10-14

- Show the Android Home app's Work agent as Online while it is idle. The app counted only legacy waiting Work sockets and active streams, but protocol v2 Work agents hold control connections and open data sockets on demand, so an idle Work agent always appeared Offline.
- Report a wrong room password in the Android Home app. The relay accepts the connection before checking the room password and then closes it, so the app looked connected but the Work agent appeared Offline and Remote Desktop could not connect. The app now shows "Wrong password" and asks you to re-enter the room password.

## 0.13.5 - 2026-10-12

- Notice a frozen relay path within 8 seconds instead of 15 during Remote Desktop sessions, on Windows, macOS, and Android. The OCI relay's network briefly stops passing traffic for about 11 seconds, roughly once an hour. A side with no data awaiting acknowledgement only noticed through its 15-second heartbeat timeout, after the Remote Desktop client had already reset the connection (about 16 seconds after data stopped). It now notices at 8 seconds and keeps retrying the resume, so the session reattaches as soon as the path recovers. Other services keep the 15-second timeout.

## 0.13.4 - 2026-10-11

- Resume Remote Desktop sessions within seconds after one side abandons a stalled POST/GET streaming transport. That transport is abandoned without a close message, so the relay kept treating it as alive until it expired about five minutes later, never told the other side to resume, and the Remote Desktop client gave up after about 20 seconds. The Azure, Go, and Python relays now treat a resume connection from either side as proof that its previous transport failed, and immediately coordinate both sides to resume.

## 0.13.3 - 2026-10-11

- Stop dropping Remote Desktop sessions that use the POST/GET streaming fallback through a proxy. On that path heartbeats routinely take about 4.7 seconds and data acknowledgements stall briefly for up to about 6 seconds before recovering on their own, but the 5-second Remote Desktop recovery deadline closed the transport first, and the session was often lost while it resumed. Remote Desktop over the streaming fallback now waits 8 seconds, on Windows, macOS, and Android; WebSocket transports keep the 5-second deadline.

## 0.13.2 - 2026-10-10

- Widen the POST/GET streaming fallback's pipelines from three uploads and two waiting GETs to five uploads and four waiting GETs, on Windows, macOS, and Android. Behind a buffering proxy each request holds its slot for about a round trip plus its travel time, so data waited whenever every slot was busy. Through a buffering proxy with a 100 ms round trip, a message every 30 ms now returns in one round trip (p50 102 ms, p99 about 105 ms, down from p50 about 154 ms and p99 about 200 ms), with smaller gains under heavier traffic and no regression.
- Keep enough idle connections for the wider pipelines so they are reused rather than re-authenticated with the proxy.

## 0.13.1 - 2026-10-09

- Keep the screen viewer working while a Remote Desktop window to the Work PC is minimized. The Windows Remote Desktop client stops the remote session from drawing its desktop while minimized, which made screen capture fail. DeskFerry Home on Windows now sets the current user's `RemoteDesktop_SuppressWhenMinimized` to 2 at startup and before it opens Remote Desktop; Remote Desktop windows that were already open apply it after reconnecting.
- When screen capture fails inside a Remote Desktop session, the viewer's error now starts with the likely cause and how to fix it.

## 0.13.0 - 2026-10-09

A redesign of every DeskFerry interface around one shared design system (`docs/design-system.md`): a calm neutral background, white cards, status chips that change colour with state, one primary action per area, sentence-case labels, and consistent spacing and typography.

- **Windows app:** the Home window, Work Services, Setup, and Screen Viewer share a new UI kit (`windows/uikit`). Each window has a header with the app icon and an overall status chip, status tiles, card panels, a painted primary button, and monospace diagnostics. Every window shows the app icon, and the app is now per-monitor DPI aware.
- **macOS Home agent:** the local control panel and screen viewer are now embedded asset files with a two-column layout, a relay list with drag-to-reorder, collapsible WinRM and diagnostics sections, and light and dark themes.
- **Android Home app:** a new platform-only UI kit with cards, status chips, ripple buttons, and styled fields; a status overview with a prominent Start/Stop button; tidier destination and relay cards; a quick-actions list; and a dark screen viewer. Destination deletion now asks for confirmation.
- **Relay dashboard:** the Azure, Go, and Python relays now serve one shared page and icon from `internal/relaydashboard`, replacing three drifting copies. The redesigned page has a live connection chip, status tiles, a responsive rooms table that becomes cards on phones, per-service session tags, room detail pages, and light and dark themes.
- **Release signing:** the Android APK is signed with the original DeskFerry release key again, so it upgrades installations of 0.12.12 and earlier. APKs from 0.12.13 to 0.12.18 were debug-signed and cannot upgrade in place.
- The Python relay and Azure relay report the current release version.

## 0.12.18 - 2026-10-09

- Bring the Android Home agent's POST/GET streaming fallback in line with the Go clients. Uploads are now finite batches instead of one never-ending POST, which buffering proxies such as Skyhigh held for about 1.5 seconds each. Up to three uploads and two waiting GETs overlap once the relay confirms pipelining, finished download batches are re-polled at once, records that arrive out of order are delivered in sequence, and upload connections are opened up front. Through a buffering proxy with a 100 ms round trip, Android's idle round trip dropped from about 1.6 s to 103 ms and its sustained median to about 165 ms.
- Raise the Android HTTP client's per-host request limit and idle connection pool so several fallback sockets' long-poll GETs and pipelined uploads do not queue behind each other.
- Report the Android app version as the current release.

## 0.12.17 - 2026-10-09

- Cut the Azure relay's per-request CPU cost. ASP.NET Core's per-request Information logs (four lines for every HTTP-stream GET and POST) are now filtered to warnings and errors, and the App Service file logger queues lines for one background writer instead of opening the log file on App Service's network storage for every line under a lock that request threads waited on. Locally, relay CPU per ping round trip dropped from 6.97 ms to 0.89 ms, and the round trip itself from 3.34 ms to 0.32 ms.
- Report the Azure relay's version as the current release.

## 0.12.16 - 2026-10-09

- Pipeline downloads on the POST/GET streaming fallback. Once the relay confirms support on a GET (`X-DeskFerry-Stream-Pipeline`), clients keep two GETs waiting, so records produced while one response is in transit leave on the other instead of waiting for the next poll.
- Each waiting pipelined GET carries only records no other response has carried. A relay re-sends records that stay unacknowledged for three seconds, in case their response was lost.
- Older clients and relays keep the single-GET behavior.

## 0.12.15 - 2026-10-09

- Pipeline uploads on the POST/GET streaming fallback. Once the relay confirms support (`X-DeskFerry-Stream-Pipeline`), clients keep up to three finite upload batches in flight, splitting large bursts into 256 KiB batches, so data queued during an upload no longer waits a full round trip for its response. Relays reorder overlapping uploads by sequence. Older relays keep receiving one batch at a time.
- Send upload batches with a known length instead of a streamed body, and pre-open the upload connections so the first data batches skip the proxy authentication round trip.
- Re-send upload batches the relay accepted but did not acknowledge within three seconds, such as one truncated by a proxy.

## 0.12.14 - 2026-10-08

- Cut latency on the POST/GET streaming fallback used through proxies that reject WebSocket upgrades. Clients now re-poll a finished downstream batch immediately instead of waiting out a 250 ms reconnect backoff (which Android let grow to five seconds), and no longer pay an `Expect: 100-continue` round trip on each upload.
- Report the received sequence on each downstream GET (`X-DeskFerry-Stream-Ack`). Updated Azure, OCI, and Python relays apply it, so batches no longer resend records the client already has, and clients skip ack-only POSTs that delayed real upstream data by a round trip.
- Relays answer a parked batch GET when their receive acknowledgement advances, after a 20 ms window that lets an immediate reply share the batch, so senders free their buffers promptly and graceful closes finish without waiting for a timeout.

## 0.12.12 - 2026-10-01

- Hedge direct resumable-session recovery across transports: after a two-second
  WebSocket resume probe, Windows and macOS clients now try the relay's HTTPS
  streaming transport instead of letting one half-open WebSocket handshake
  consume most of the RDP automatic-reconnect window.

## 0.12.11 - 2026-09-28

- Limit each resumable relay dial attempt to eight seconds so a half-open TCP or proxy attempt cannot consume nearly the entire Windows RDP automatic-reconnect window.
- Apply the shorter retry cadence consistently to Windows, macOS, Work, and Android agents while retaining the five-minute logical-session recovery window.

## 0.12.10 - 2026-09-20

- Add negotiated application-level heartbeats to Work-agent control channels so Azure, OCI, and Python relays can promptly replace connections that a proxy keeps open after their data path has stalled.
- Preserve compatibility with older relays by enabling control heartbeats only when the relay advertises support in its protocol-v2 greeting.

## 0.12.9 - 2026-09-19

- Keep the Android CPU awake only while an RDP or SMB bridge is active so its heartbeat and resume workers are not suspended during an ongoing remote session.
- Remove OkHttp's redundant ten-second WebSocket ping deadline on Android; DeskFerry's end-to-end heartbeat remains responsible for detecting a stalled relay path without dropping mobile links after one delayed pong.

## 0.12.8 - 2026-09-19

- Coordinate resumable relay recovery across both endpoints when either transport fails, preventing a stale-but-apparently-connected peer from receiving replay data at an incompatible offset and forcing the underlying RDP session to reconnect.
- Add relay regression coverage for one-sided transport stalls and repeated attachments while the opposite side is recovering.

## 0.12.7 - 2026-09-18

- Replace an RDP relay transport after five seconds without acknowledgement progress, while retaining the ten-second deadline for WinRM and SMB. This preserves enough of the RDP client's reconnect window when a replacement relay attachment is itself delayed.
- Apply the shorter RDP recovery deadline consistently to Windows, macOS, and Android Home agents and Work agents.

## 0.12.6 - 2026-09-17

- Coalesce overlapping Work and Home resume attachments before restarting a relay bridge, preventing replay traffic from briefly crossing mismatched transport generations and terminating the recovered RDP session with a sequence-offset error.
- Apply the overlapping-resume fix consistently to the Azure, Go/OCI, and Python relay implementations, with a regression test covering both sides replacing stalled transports in quick succession.

## 0.12.3 - 2026-09-08

- Preserve the unaffected half of a resumable relay bridge when only the Work or Home transport drops, allowing that endpoint alone to reattach without exposing a relay-generated EOF to the still-connected RDP client.
- Bring the Python relay to the same one-sided resume behavior and diagnostic attachment logging, and make the Go and Python HTTP-stream servers recover stale client sequence state after a relay restart instead of repeatedly rejecting the upload.
- Retire unresponsive protocol-v2 Work control connections after an unanswered offer in the Go and Python relays, matching the Azure relay while leaving their separate active data sessions untouched.
- Show the actual connected RDP relay prominently in Windows Home, separately from connections still establishing. Combine status from all configured relays and recognize idle protocol-v2 work controls.
- Retire an Azure work control connection after an unanswered session offer so later attempts can use a fresh control channel, without deliberately terminating separate active data sessions.
- Recover HTTP-stream clients whose sequence state outlives an Azure restart: deliver an explicit reconnect close at the client's expected sequence instead of repeatedly failing uploads with HTTP 500.
- Reset resumable-session retry backoff as soon as a restarted relay is reachable, so a provisional resume attachment that closes before its peer arrives is retried promptly instead of adding several seconds that can exceed the local RDP client's reconnect tolerance.

## 0.12.2 - 2026-09-08

- Consolidate bursts of Android connectivity callbacks before proactively replacing relay transports, preventing a mobile network handoff from repeatedly canceling new RDP resume and connection attempts. Transport failures continue to trigger immediate recovery.

## 0.12.1 - 2026-09-04

- Preserve the selected Home profile's relay URLs, room proof, SMB alias, and explicit proxy when migrating a legacy Windows network service whose stored settings differ.
- Keep Work Services optional and independent from the selected Home destination: load its own editable installed room, preserve relay bases internally without showing redundant controls, and make the uninstalled state explicit.
- Keep the Work Services window compact, use balanced outer margins, and give the remaining window height to its Activity log without an always-visible scrollbar.
- Remove the redundant Windows Components window and installation-location control; Home SMB/UNC settings remain in the main control panel while setup stays an internal elevated/CLI backend.
- Shorten the main-window button to **Work Services**, state **admin required** on the RDP decoding apply action, and correctly forward debug-build switches when assembling the self-contained Windows payload.
- Make the Work Services Activity pane show a recent tail of the Work service diagnostic log and follow live relay, RDP, WinRM, SMB, and screen-session events across log rotation.

## 0.12.0 - 2026-09-04

- Replace the separate Windows Home, Work agent, Work configurator, Home network, and setup artifacts with one self-contained multi-mode `DeskFerry.exe`.
- Reuse each named Windows connection profile's room, ordered relay bases, and proxy for Home and Work configuration, migrate an existing Work service connection as a **This PC** profile, and preserve DPAPI-protected room credentials.
- Add in-app Work Services and Windows Components controls, per-profile proxy settings, optional WinRM/SMB/screenshot switches, and service-aware actions that offer **Install** only when a service is absent.
- Register the Work and optional Home SMB services against the merged executable, embed the verified Wintun/tun2socks payload, and migrate legacy service paths and files during upgrade.
- Simplify Windows builds and releases to optimized and debug variants of the single executable.

## 0.11.4 - 2026-09-02

- Authenticate HTTPS `POST`/`GET` fallback tunnels through NTLM proxies with the current Windows identity, allowing the Azure relay to remain usable when a gateway permits CONNECT but strips WebSocket upgrades.
- Split the saved proxy capability into “prefer HTTP streams” and “CONNECT unsupported,” and migrate v0.11.3 state without incorrectly bypassing HTTPS relays.
- Classify proxy authentication failures, CONNECT rejections, and WebSocket-upgrade rejections separately in connection logs.

## 0.11.3 - 2026-09-01

- Bound proxy WebSocket/CONNECT probes so a gateway that holds a rejected upgrade cannot consume the HTTP-stream fallback's entire setup budget.
- Persist explicit proxies that have demonstrated a working plain-HTTP `POST`/`GET` fallback, letting later Home connections and process starts skip CONNECT and immediately bypass incompatible HTTPS relays.
- Align the Room password field and Password options checkbox with the other Windows Home connection rows.

## 0.11.2 - 2026-09-01

- Rework the Windows Home connection form as a DPI-safe two-column layout so labels sit beside full-width fields and destination and relay controls no longer clip at display scaling.

## 0.11.1 - 2026-09-01

- Authenticate plain-HTTP fallback requests through NTLM forward proxies with the current Windows identity, including every replacement `POST` and `GET` connection; LocalSystem services securely acquire the credential from a logged-on interactive session instead of presenting the rejected machine account.
- Request replay-safe finite HTTP batches so proxies that buffer endless uploads or downloads release control and data records promptly, while retaining persistent streams for older clients and non-buffering paths.
- Extend the opt-in external compatibility probe and verify protocol-v2 pairing and data transfer through an authenticated Skyhigh Secure Web Gateway that rejects WebSocket `CONNECT`.
- Show the effective proxy URL, selected wire protocol, and relay protocol in Windows Home activity messages without exposing proxy credentials or the unresolved `env` configuration keyword.
- Rebalance the Windows Home destination row so the profile selector stays compact and the adjacent editable destination-name field remains usable.
- Simplify the RDP graphics-decoding row to the mode selector and apply button, and report the applied Windows policy alongside the recommendation instead of in a redundant field.
- Serialize elevated SMB profile reconfiguration requests so rapid destination/save events cannot run overlapping installers and leave the optional network service removed.

## 0.11.0 - 2026-09-01

- Fall back automatically to a pair of persistent HTTP streams when a configured forward proxy rejects the WebSocket `CONNECT` path: a chunked `POST` carries upstream messages and a streaming `GET` carries downstream messages.
- Reconnect either HTTP half independently with sequence acknowledgements, bounded replay buffers, duplicate suppression, and keepalive records; automatically rotate an unacknowledged upload so managed front ends that buffer chunked requests release the batch.
- Preserve control, presence, diagnostic, RDP, WinRM, SMB, screen, and resumable-session messages across proxy timeouts.
- Add the compatible HTTP-stream endpoint to the Azure .NET, OCI Go, and Python relays, and use the fallback from the Windows Work agent plus Windows, macOS, and Android Home clients.
- Keep native WebSockets as the preferred transport and retain `-proxy direct` as the explicit bypass; HTTPS relays still require a proxy capable of `CONNECT`, so a non-CONNECT path must use a configured plain-HTTP relay such as the OCI fallback.

## 0.10.22 - 2026-08-30

- Make the Azure relay notify both endpoints with a service-restart close frame before aborting an interrupted resumable bridge, so the surviving endpoint immediately reattaches instead of remaining unaware behind IIS or an Azure frontend until RDP disconnects.

## 0.10.21 - 2026-08-30

- Add an administrator-applied Windows Home setting for Automatic, Hardware, or Software RDP graphics decoding without interrupting active sessions.
- Recommend software decoding when recent RDP socket-abort disconnects coincide with a display driver more than three years old, while leaving the final policy choice to the user.

## 0.10.20 - 2026-08-29

- Keep Windows MSTSC automatic reconnection trying for the supported maximum of 200 attempts, so a longer Home-network or DNS interruption is less likely to exhaust the generated DeskFerry RDP profile's retry window.

## 0.10.19 - 2026-08-28

- Detect silently blackholed resumable transports after a 5-second idle interval and a 15-second unanswered heartbeat instead of waiting up to roughly a minute before starting transparent reconnection.
- Use the same faster failure detection in the Android Home agent so mobile, Windows, and macOS clients recover on comparable timelines.
- Mark generated Windows loopback RDP profiles as fixed-LAN connections and disable MSTSC network and bandwidth auto-detection, preventing physical-interface changes from being applied to the local `127.0.0.1` tunnel.
- Preflight running-file locks before a Windows Home upgrade removes the optional network service, so an open Home GUI cannot leave file access stopped after a failed executable replacement.

## 0.10.18 - 2026-08-26

- Keep relay sessions resumable when one WebSocket pump fails while the canceled opposite pump concurrently observes a logical-close status, instead of prematurely completing the session and rejecting the reconnecting endpoint.
- Record both pumps' end state and close details for interrupted resumable Azure and Python relay sessions so future transport failures identify the initiating direction.

## 0.10.17 - 2026-08-22

- Reuse a bounded HTTP connection pool for Windows and macOS Home relay-status checks instead of leaving a new idle connection behind after every poll.
- Prevent overlapping Windows Home status checks and close retired pooled connections when proxy settings change or the app exits, avoiding gradual outbound-port exhaustion that can disrupt unrelated RDP traffic.

## 0.10.16 - 2026-08-21

- Acknowledge relay-initiated WebSocket closes as soon as Android receives them, allowing completed RDP sessions and rejected resumes to release their stale loopback connection immediately instead of waiting through repeated 20-second resume attempts.

## 0.10.15 - 2026-08-21

- Deliver relay-to-local Android RDP and SMB data on a dedicated ordered writer so a backgrounded or stalled local client cannot block WebSocket failure callbacks and delay transparent resumption for minutes.
- Bound Android's pending local-delivery buffer and acknowledge resumable data only after it reaches the local client, preserving replay safety without allowing unbounded memory growth.
- Retain short-lived completed-session tombstones in every relay implementation so a finished session is rejected immediately instead of being reconstructed as a phantom session that causes repeated resume timeouts and stuck manual connection attempts.

## 0.10.14 - 2026-08-20

- Explicitly enable MSTSC automatic reconnection in generated Windows Home RDP profiles so a transient tunnel or network interruption does not depend on the user's saved Remote Desktop client preference.
- Encode CLI WinRM worker output and errors as UTF-8 base64 inside its JSON protocol, preventing event logs and other Windows text containing backslashes or control characters from corrupting diagnostic command responses.

## 0.10.13 - 2026-08-19

- Reconstruct authenticated resumable relay sessions after a relay process restart so active RDP, WinRM, and SMB streams can reattach using their endpoint-owned replay buffers instead of failing with `unknown resumable session`.
- Return a retryable response when the Home-side resume reaches a restarted relay before the Work side has restored the protected room, while keeping invalid credentials terminal.
- Keep Android resume deadlines responsive when its local RDP consumer stalls by moving potentially blocking loopback writes out of the resume-state lock.

## 0.10.12 - 2026-08-18

- Add negotiated end-to-end heartbeats to resumable RDP, WinRM, and SMB streams so silently stalled relay paths are replaced automatically without closing the local TCP connection.
- Keep heartbeat framing compatible with rolling upgrades by enabling it only after both Home and Work agents confirm support through the relay.
- Add the same heartbeat and transparent-resume behavior to the Android Home agent.
- Attribute tunnel shutdowns to the first endpoint that actually ended, including orderly EOFs.

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
