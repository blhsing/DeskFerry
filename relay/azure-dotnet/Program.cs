using System.Buffers;
using System.Collections.Concurrent;
using System.Diagnostics;
using System.Net.WebSockets;
using System.Text;
using System.Threading.Channels;

var builder = WebApplication.CreateBuilder(args);
builder.Logging.ClearProviders();
builder.Logging.AddSimpleConsole(options =>
{
    options.SingleLine = true;
    options.TimestampFormat = "yyyy-MM-ddTHH:mm:ss.fffZ ";
    options.UseUtcTimestamp = true;
});
builder.Services.AddSingleton<RelayHub>();

var app = builder.Build();
app.UseWebSockets(new WebSocketOptions
{
    KeepAliveInterval = TimeSpan.FromSeconds(20)
});

app.MapGet("/", () => Results.Redirect("/relay/"));
app.MapGet("/relay", () => Results.Text(DashboardHtml(), "text/html; charset=utf-8"));
app.MapGet("/relay/health", () => Results.Json(new
{
    status = "ok",
    service = "DeskFerry.Relay",
    time = DateTimeOffset.UtcNow
}));
app.MapGet("/relay/icon.svg", () => Results.Text(IconSvg(), "image/svg+xml; charset=utf-8"));
app.MapGet("/relay/status", (HttpContext context, RelayHub hub) => Results.Json(hub.Snapshot(context.Request.Query["room"].FirstOrDefault())));
app.MapGet("/relay/{room}", (string room) => Results.Text(DashboardHtml(room), "text/html; charset=utf-8"));

app.Map("/relay/ws", RelayWebSocketHandler);
app.Map("/relay/{room}/ws", RelayWebSocketHandler);

async Task RelayWebSocketHandler(HttpContext context, RelayHub hub)
{
    if (!context.WebSockets.IsWebSocketRequest)
    {
        context.Response.StatusCode = StatusCodes.Status426UpgradeRequired;
        await context.Response.WriteAsync("WebSocket upgrade required.");
        return;
    }

    var role = ReadRole(context.Request);
    var token = ReadRoom(context) ?? (role == "dashboard" ? "dashboard" : ReadToken(context.Request));
    if (role is null || token is null)
    {
        context.Response.StatusCode = StatusCodes.Status401Unauthorized;
        await context.Response.WriteAsync("Missing relay role or bearer token.");
        return;
    }

    using var socket = await context.WebSockets.AcceptWebSocketAsync();
    var remote = RemoteAddress(context);
    var roomForLog = ReadRoom(context) ?? (role == "dashboard" ? "overview" : "header-token");
    app.Logger.LogInformation("websocket connected role={Role} room={Room} remote={Remote} user_agent={UserAgent}", role, roomForLog, remote, context.Request.Headers.UserAgent.ToString());
    switch (role)
    {
        case "dashboard":
            await hub.ServeDashboardAsync(socket, remote, ReadRoom(context), context.RequestAborted);
            break;
        case "agent":
            await hub.ServeAgentAsync(token, socket, remote, ReadAgentIdentity(context.Request), ReadResumable(context.Request), context.RequestAborted);
            break;
        case "client":
            await hub.ServeClientAsync(token, socket, remote, ReadResumable(context.Request), context.RequestAborted);
            break;
        case "resume":
            await hub.ServeResumeAsync(token, socket, remote, context.Request.Headers["X-DeskFerry-Session"].FirstOrDefault(), context.Request.Headers["X-DeskFerry-Session-Side"].FirstOrDefault(), context.RequestAborted);
            break;
        case "home-agent":
            await hub.ServeHomeAgentAsync(token, socket, remote, context.RequestAborted);
            break;
        case "probe":
            await socket.CloseAsync(WebSocketCloseStatus.NormalClosure, "probe ok", CancellationToken.None);
            break;
        default:
            await socket.CloseAsync(WebSocketCloseStatus.PolicyViolation, "unsupported role", CancellationToken.None);
            break;
    }
}

app.Run();

static string? ReadRole(HttpRequest request)
{
    var role = request.Headers["X-DeskFerry-Role"].FirstOrDefault()
        ?? request.Headers["X-TunnelDesktop-Role"].FirstOrDefault()
        ?? request.Query["role"].FirstOrDefault();
    role = role?.Trim().ToLowerInvariant();
    return role is "agent" or "client" or "home-agent" or "probe" or "dashboard" or "resume" ? role : null;
}

static bool ReadResumable(HttpRequest request)
{
    var value = request.Headers["X-DeskFerry-Resumable"].FirstOrDefault()?.Trim();
    return value == "1" || string.Equals(value, "true", StringComparison.OrdinalIgnoreCase);
}

static string? ReadToken(HttpRequest request)
{
    var auth = request.Headers.Authorization.FirstOrDefault();
    if (!string.IsNullOrWhiteSpace(auth) && auth.StartsWith("Bearer ", StringComparison.OrdinalIgnoreCase))
    {
        var token = auth["Bearer ".Length..].Trim();
        return string.IsNullOrWhiteSpace(token) ? null : token;
    }
    var queryToken = request.Query["token"].FirstOrDefault()?.Trim();
    if (queryToken is { Length: > 0 })
    {
        return queryToken;
    }
    var room = request.Query["room"].FirstOrDefault()?.Trim();
    return string.IsNullOrWhiteSpace(room) ? "default" : room;
}

static string? ReadRoom(HttpContext context)
{
    var value = context.Request.RouteValues["room"]?.ToString()?.Trim();
    return string.IsNullOrWhiteSpace(value) ? null : value;
}

static string RemoteAddress(HttpContext context)
{
    var forwarded = context.Request.Headers["X-Forwarded-For"].FirstOrDefault();
    if (!string.IsNullOrWhiteSpace(forwarded))
    {
        return forwarded.Split(',')[0].Trim();
    }
    return context.Connection.RemoteIpAddress?.ToString() ?? "unknown";
}

static AgentIdentity ReadAgentIdentity(HttpRequest request)
{
    return new AgentIdentity(
        CleanAgentIdentity(request.Headers["X-DeskFerry-Agent-Instance"].FirstOrDefault()),
        CleanAgentIdentity(request.Headers["X-DeskFerry-Agent-Slot"].FirstOrDefault()));
}

static string CleanAgentIdentity(string? value)
{
    var raw = value?.Trim() ?? "";
    if (raw.Length == 0)
    {
        return "";
    }
    var builder = new StringBuilder(Math.Min(raw.Length, 64));
    foreach (var c in raw)
    {
        if (builder.Length >= 64)
        {
            break;
        }
        if (c is >= 'A' and <= 'Z' or >= 'a' and <= 'z' or >= '0' and <= '9' or '-' or '_' or '.')
        {
            builder.Append(c);
        }
    }
    return builder.ToString();
}

static string IconSvg() => """
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 108 108">
  <defs>
    <linearGradient id="bg" x1="12" y1="12" x2="96" y2="96" gradientUnits="userSpaceOnUse">
      <stop stop-color="#13324d"/>
      <stop offset="1" stop-color="#40b5ae"/>
    </linearGradient>
    <clipPath id="clip">
      <rect x="6" y="6" width="96" height="96" rx="22"/>
    </clipPath>
  </defs>
  <rect x="6" y="6" width="96" height="96" rx="22" fill="url(#bg)"/>
  <g clip-path="url(#clip)">
    <path d="M6 34c22-17 61-14 97-24l3 12c-32 12-70 9-99 23z" fill="#fff" opacity=".08"/>
  </g>
  <path d="M12 35c19-13 38-6 56-18M43 97c16-13 37-6 60-19" fill="none" stroke="#fff" stroke-width="1.2" stroke-linecap="round" opacity=".22"/>
  <path d="M70 31c12-8 22-4 33-12" fill="none" stroke="#fff" stroke-width=".7" stroke-linecap="round" opacity=".18"/>
  <path d="M27 28q0-7 7-7h40q7 0 7 7v28q0 7-7 7H34q-7 0-7-7z" fill="#031727" opacity=".22"/>
  <path d="M27 25q0-7 7-7h40q7 0 7 7v28q0 7-7 7H34q-7 0-7-7z" fill="#fff"/>
  <path d="M34 27q0-3 3-3h34q3 0 3 3v20q0 3-3 3H37q-3 0-3-3z" fill="#17324d"/>
  <path d="M38 27h12l-9 23h-7z" fill="#fff" opacity=".14"/>
  <path d="M40 29h26" fill="none" stroke="#fff" stroke-width=".65" stroke-linecap="round" opacity=".20"/>
  <path d="M49 59h10l3 8H46zM39 68q0-3 3-3h24q3 0 3 3v3H39z" fill="#fff"/>
  <path d="M20 67h68l-8 11q-9 7-42 4q-9-2-18-15z" fill="#031727" opacity=".20"/>
  <path d="M20 64h68l-8 11q-9 7-42 4q-9-2-18-15z" fill="#e66d4f"/>
  <path d="M38 77c12 4 28 3 42-2" fill="none" stroke="#71323a" stroke-width=".8" stroke-linecap="round" opacity=".28"/>
  <path d="M31 66h43q2 0 2 2t-2 2H31q-2 0-2-2t2-2z" fill="#fff" opacity=".76"/>
  <g clip-path="url(#clip)">
    <path d="M0 78q13-7 27 0t28 0t28 0q13 7 25-2v32H0z" fill="#69d2c7"/>
    <path d="M4 86q18-7 36 0t36 0q16-6 28-2v4q-13-2-28 3q-18 7-36 0q-18-7-36 0z" fill="#fff" opacity=".48"/>
    <path d="M17 92c8-3 15-2 22 0M73 96c7-3 15-2 21-5" fill="none" stroke="#fff" stroke-width=".65" stroke-linecap="round" opacity=".36"/>
    <path d="M14 97c20-5 31 3 52-2" fill="none" stroke="#fff" stroke-width=".8" stroke-linecap="round" opacity=".32"/>
  </g>
</svg>
""";

static string DashboardHtml(string room = "")
{
    var roomJson = System.Text.Json.JsonSerializer.Serialize(room);
    return $$"""
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>DeskFerry Relay</title>
  <link rel="icon" href="/relay/icon.svg" type="image/svg+xml">
  <style>
    :root {
      color-scheme: light;
      --bg: #f5f7f8;
      --panel: #ffffff;
      --ink: #1f2933;
      --muted: #65717d;
      --line: #d7dee3;
      --accent: #2f6f73;
      --ok: #287d52;
      --warn: #9a6a12;
      --bad: #a94343;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: "Segoe UI", system-ui, -apple-system, BlinkMacSystemFont, sans-serif;
      background: var(--bg);
      color: var(--ink);
    }
    header {
      padding: 28px 24px 18px;
      border-bottom: 1px solid var(--line);
      background: var(--panel);
    }
    main {
      width: min(1120px, calc(100% - 32px));
      margin: 22px auto 40px;
    }
    h1 {
      margin: 0 0 6px;
      font-size: clamp(26px, 4vw, 38px);
      letter-spacing: 0;
    }
    .brand {
      display: flex;
      align-items: center;
      gap: 14px;
    }
    .brand-icon {
      width: 58px;
      height: 58px;
      flex: 0 0 58px;
      border-radius: 13px;
    }
    .brand-text { min-width: 0; }
    .subtle { color: var(--muted); }
    .toolbar {
      display: flex;
      gap: 10px;
      align-items: center;
      flex-wrap: wrap;
      margin-top: 16px;
    }
    .toolbar input {
      flex: 1 1 360px;
      min-width: 0;
      height: 40px;
      border: 1px solid var(--line);
      border-radius: 8px;
      padding: 0 12px;
      color: var(--ink);
      background: #fbfcfd;
      font: 13px ui-monospace, SFMono-Regular, Consolas, monospace;
    }
    .toolbar button {
      height: 40px;
      border: 1px solid var(--accent);
      border-radius: 8px;
      padding: 0 14px;
      color: var(--accent);
      background: #fff;
      font-weight: 700;
      cursor: pointer;
    }
    .grid {
      display: grid;
      grid-template-columns: repeat(3, minmax(0, 1fr));
      gap: 14px;
      margin-bottom: 18px;
    }
    .card {
      background: var(--panel);
      border: 1px solid var(--line);
      border-radius: 8px;
      padding: 16px;
      min-height: 128px;
    }
    .label {
      color: var(--muted);
      font-size: 13px;
      font-weight: 700;
      text-transform: uppercase;
    }
    .value {
      margin-top: 10px;
      font-size: 28px;
      font-weight: 700;
      line-height: 1.1;
    }
    .ok { color: var(--ok); }
    .warn { color: var(--warn); }
    .bad { color: var(--bad); }
    table {
      width: 100%;
      border-collapse: collapse;
      background: var(--panel);
      border: 1px solid var(--line);
      border-radius: 8px;
      overflow: hidden;
    }
    th, td {
      padding: 12px 14px;
      text-align: left;
      border-bottom: 1px solid var(--line);
      vertical-align: top;
      font-size: 14px;
    }
    th {
      color: var(--muted);
      font-size: 12px;
      text-transform: uppercase;
      background: #fbfcfd;
    }
    tr:last-child td { border-bottom: 0; }
    code {
      font-family: ui-monospace, SFMono-Regular, Consolas, monospace;
      font-size: 13px;
    }
    .pill {
      display: inline-block;
      padding: 3px 8px;
      border-radius: 999px;
      border: 1px solid var(--line);
      font-size: 12px;
      font-weight: 700;
      background: #f9fafb;
    }
    .pill.ok { border-color: #bfe4cf; background: #edf8f1; }
    .pill.bad { border-color: #efc5c5; background: #fff0f0; }
    @media (max-width: 760px) {
      .grid { grid-template-columns: 1fr; }
      th:nth-child(5), td:nth-child(5) { display: none; }
      .brand-icon {
        width: 48px;
        height: 48px;
        flex-basis: 48px;
      }
    }
  </style>
</head>
<body>
  <header>
    <div class="brand">
      <img class="brand-icon" src="/relay/icon.svg" alt="">
      <div class="brand-text">
        <h1>DeskFerry Relay</h1>
        <div class="subtle">Azure WebSocket relay at <code>/relay/ws</code>. Status updates stream live over WebSocket.</div>
      </div>
    </div>
    <div class="toolbar">
      <input id="roomUrl" readonly aria-label="Relay room URL">
      <button id="copyRoom" type="button">Copy</button>
    </div>
  </header>
  <main>
    <section class="grid">
      <div class="card">
        <div class="label">Work agent</div>
        <div id="workStatus" class="value warn">Checking</div>
        <p id="workDetail" class="subtle">Waiting for status.</p>
      </div>
      <div class="card">
        <div class="label">Home side</div>
        <div id="homeStatus" class="value warn">Checking</div>
        <p id="homeDetail" class="subtle">Waiting for status.</p>
      </div>
      <div class="card">
        <div class="label">RDP streams</div>
        <div id="streamStatus" class="value">0</div>
        <p id="streamDetail" class="subtle">No active pairs.</p>
      </div>
    </section>
    <table>
      <thead>
        <tr>
          <th>Room</th>
          <th>Work Agent</th>
          <th>Home Side</th>
          <th>Active Pairs</th>
          <th>Last Client</th>
        </tr>
      </thead>
      <tbody id="rooms">
        <tr><td colspan="5" class="subtle">Loading relay status...</td></tr>
      </tbody>
    </table>
  </main>
  <script>
    const roomsBody = document.getElementById("rooms");
    const workStatus = document.getElementById("workStatus");
    const workDetail = document.getElementById("workDetail");
    const homeStatus = document.getElementById("homeStatus");
    const homeDetail = document.getElementById("homeDetail");
    const streamStatus = document.getElementById("streamStatus");
    const streamDetail = document.getElementById("streamDetail");
    const roomUrl = document.getElementById("roomUrl");
    const copyRoom = document.getElementById("copyRoom");
    const pageRoom = {{roomJson}};

    function pill(ok, text) {
      return `<span class="pill ${ok ? "ok" : "bad"}">${text}</span>`;
    }

    function esc(value) {
      return String(value ?? "").replace(/[&<>"']/g, char => ({
        "&": "&amp;",
        "<": "&lt;",
        ">": "&gt;",
        '"': "&quot;",
        "'": "&#39;"
      }[char]));
    }

    function fmt(value) {
      if (!value) return "";
      return new Date(value).toLocaleString();
    }

    function setValue(node, text, cls) {
      node.className = "value " + cls;
      node.textContent = text;
    }

    function relayRoomUrl(room) {
      if (!room) return `${location.origin}/relay/`;
      return `${location.origin}/relay/${encodeURIComponent(room)}`;
    }

    function setRoomUrl(room) {
      roomUrl.value = relayRoomUrl(room);
    }

    function render(data) {
      try {
        const rooms = data.rooms || [];
        const waitingAgents = rooms.reduce((sum, r) => sum + (r.waiting_agents || 0), 0);
        const activePairs = rooms.reduce((sum, r) => sum + (r.active_pairs || 0), 0);
        const homeAgents = rooms.filter(r => r.home_agent_connected).length;
        const homeActiveRooms = rooms.filter(r => r.home_agent_connected || (r.active_pairs || 0) > 0).length;

        setValue(workStatus, waitingAgents + activePairs > 0 ? "Connected" : "Waiting", waitingAgents + activePairs > 0 ? "ok" : "warn");
        workDetail.textContent = `${waitingAgents} idle work sockets, ${activePairs} paired streams.`;
        setValue(homeStatus, homeActiveRooms > 0 ? "Active" : "Waiting", homeActiveRooms > 0 ? "ok" : "warn");
        homeDetail.textContent = `${homeAgents} presence socket${homeAgents === 1 ? "" : "s"}, ${activePairs} active RDP stream${activePairs === 1 ? "" : "s"}.`;
        streamStatus.textContent = activePairs.toString();
        streamDetail.textContent = activePairs === 0 ? "No active RDP streams." : `${activePairs} RDP stream${activePairs === 1 ? "" : "s"} bridged.`;

        if (rooms.length === 0) {
          roomsBody.innerHTML = '<tr><td colspan="5" class="subtle">No token rooms have connected yet.</td></tr>';
          return;
        }
        roomsBody.innerHTML = rooms.map(r => {
          const workConnected = (r.waiting_agents || 0) + (r.active_pairs || 0) > 0;
          const homePresence = !!r.home_agent_connected;
          const streamActive = (r.active_pairs || 0) > 0;
          const homeState = homePresence ? "presence" : (streamActive ? "active stream" : "waiting");
          const homeInfo = homePresence
            ? `${esc(r.home_agent_remote || "")}<br>${esc(fmt(r.home_agent_connected_at))}`
            : `${r.active_pairs || 0} active<br>${esc(fmt(r.last_client_connected_at))}`;
          return `<tr>
            <td><code>${esc(r.id)}</code></td>
            <td>${pill(workConnected, workConnected ? "connected" : "waiting")}<br><span class="subtle">${r.waiting_agents || 0} idle<br>${esc(fmt(r.last_agent_connected_at))}</span></td>
            <td>${pill(homePresence || streamActive, homeState)}<br><span class="subtle">${homeInfo}</span></td>
            <td>${r.active_pairs || 0}<br><span class="subtle">${r.total_pairs || 0} total</span></td>
            <td><span class="subtle">${esc(r.last_client_remote || "")}<br>${esc(fmt(r.last_client_connected_at))}</span></td>
          </tr>`;
        }).join("");
      } catch (error) {
        setValue(workStatus, "Error", "bad");
        setValue(homeStatus, "Error", "bad");
        workDetail.textContent = error.message;
        homeDetail.textContent = error.message;
        roomsBody.innerHTML = `<tr><td colspan="5" class="bad">${error.message}</td></tr>`;
      }
    }

    function connectDashboard() {
      const scheme = location.protocol === "https:" ? "wss:" : "ws:";
      const roomPath = pageRoom ? `/relay/${encodeURIComponent(pageRoom)}/ws` : "/relay/ws";
      const socket = new WebSocket(`${scheme}//${location.host}${roomPath}?role=dashboard`);
      socket.onopen = () => {
        workDetail.textContent = "Connected to live relay status.";
        homeDetail.textContent = "Connected to live relay status.";
      };
      socket.onmessage = event => render(JSON.parse(event.data));
      socket.onclose = () => {
        setValue(workStatus, "Reconnecting", "warn");
        setValue(homeStatus, "Reconnecting", "warn");
        workDetail.textContent = "Dashboard status socket closed. Reconnecting...";
        homeDetail.textContent = "Dashboard status socket closed. Reconnecting...";
        setTimeout(connectDashboard, 1500);
      };
      socket.onerror = () => socket.close();
    }

    connectDashboard();
    setRoomUrl(pageRoom);
    copyRoom.addEventListener("click", async () => {
      roomUrl.select();
      await navigator.clipboard.writeText(roomUrl.value);
    });
  </script>
</body>
</html>
""";
}

sealed class RelayHub
{
    private const string Started = "started";
    private const string AgentUnavailable = "agent-unavailable";
    private const string ClientUnavailable = "client-unavailable";

    private readonly ConcurrentDictionary<string, RelayRoom> _rooms = new();
    private readonly ConcurrentDictionary<Guid, DashboardClient> _dashboards = new();
    private readonly ConcurrentDictionary<string, ResumeSession> _sessions = new();
    private readonly ILogger<RelayHub> _log;

    public RelayHub(ILogger<RelayHub> log)
    {
        _log = log;
    }

    public async Task ServeAgentAsync(string token, WebSocket socket, string remote, AgentIdentity identity, bool resumable, CancellationToken abort)
    {
        var room = RoomFor(token);
        var (waiting, replaced) = room.EnqueueAgent(socket, remote, identity, resumable);
        _log.LogInformation("agent waiting room={Room} remote={Remote} key={AgentKey} replaced={Replaced}", room.Id, remote, identity.LogString, replaced);
        NotifyDashboards();

        HomePeer? peer = null;
        using var reg = abort.Register(() => waiting.TryCancel());
        try
        {
            peer = await waiting.WaitAsync();
            _log.LogInformation("pairing room={Room} agent={AgentRemote} client={ClientRemote}", room.Id, remote, peer.Remote);
            if (waiting.Resumable && peer.Resumable)
            {
                var session = NewResumeSession(room, remote, peer.Remote);
                if (!await TrySendControlAsync(socket, room.Id, remote, "agent", $"start {session.Id}", CancellationToken.None))
                {
                    peer.Started.TrySetResult(AgentUnavailable);
                    session.Finish();
                    return;
                }
                if (!await TrySendControlAsync(peer.Socket, room.Id, peer.Remote, "client", $"start {session.Id}", CancellationToken.None))
                {
                    peer.Started.TrySetResult(ClientUnavailable);
                    peer.Done.TrySetResult();
                    session.Finish();
                    return;
                }
                peer.Started.TrySetResult(Started);
                await session.RunAsync(socket, peer.Socket, peer.Done, NotifyDashboards);
                return;
            }
            if (!await TrySendStartAsync(socket, room.Id, remote, "agent", abort))
            {
                peer.Started.TrySetResult(AgentUnavailable);
                return;
            }
            if (!await TrySendStartAsync(peer.Socket, room.Id, peer.Remote, "client", abort))
            {
                peer.Started.TrySetResult(ClientUnavailable);
                peer.Done.TrySetResult();
                return;
            }
            peer.Started.TrySetResult(Started);
            await room.BridgeAsync(socket, peer.Socket, remote, peer.Remote, peer.Done, NotifyDashboards, abort);
        }
        catch (OperationCanceledException)
        {
            peer?.Started.TrySetCanceled();
        }
        catch (WebSocketException ex)
        {
            _log.LogInformation(ex, "agent websocket ended room={Room} remote={Remote}", room.Id, remote);
            peer?.Started.TrySetResult(AgentUnavailable);
        }
        catch (Exception ex)
        {
            _log.LogInformation(ex, "agent websocket ended room={Room} remote={Remote}", room.Id, remote);
            peer?.Started.TrySetResult(AgentUnavailable);
        }
        finally
        {
            peer?.Started.TrySetResult(AgentUnavailable);
            room.RemoveWaiting(waiting);
            NotifyDashboards();
        }
    }

    public async Task ServeClientAsync(string token, WebSocket socket, string remote, bool resumable, CancellationToken abort)
    {
        var room = RoomFor(token);
        while (socket.State == WebSocketState.Open && !abort.IsCancellationRequested)
        {
            var peer = room.TryTakeAgent();
            if (peer is null)
            {
                _log.LogInformation("client rejected without agent room={Room} remote={Remote}", room.Id, remote);
                await socket.CloseAsync(WebSocketCloseStatus.EndpointUnavailable, "no work agent connected", CancellationToken.None);
                return;
            }

            var done = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            var started = new TaskCompletionSource<string>(TaskCreationOptions.RunContinuationsAsynchronously);
            if (!peer.TryPair(new HomePeer(socket, remote, done, started, resumable)))
            {
                done.TrySetResult();
                continue;
            }
            NotifyDashboards();

            using var reg = abort.Register(() =>
            {
                started.TrySetCanceled();
                done.TrySetCanceled();
            });
            try
            {
                var startResult = await started.Task;
                if (startResult == Started)
                {
                    await done.Task;
                    return;
                }
                if (startResult == ClientUnavailable)
                {
                    return;
                }
                _log.LogInformation("skipped unavailable work agent room={Room} agent={AgentRemote} client={ClientRemote}", room.Id, peer.Remote, remote);
            }
            catch (OperationCanceledException)
            {
                done.TrySetCanceled();
                return;
            }
        }

        await CloseQuietlyAsync(socket);
    }

    public async Task ServeResumeAsync(string token, WebSocket socket, string remote, string? sessionId, string? side, CancellationToken abort)
    {
        sessionId = CleanSessionValue(sessionId);
        side = side?.Trim().ToLowerInvariant();
        var roomId = RoomId(token);
        var key = $"{roomId}/{sessionId}";
        if (sessionId.Length == 0 || side is not ("agent" or "client") || !_sessions.TryGetValue(key, out var session))
        {
            _log.LogInformation("resume rejected room={Room} session={Session} side={Side} remote={Remote}", roomId, sessionId, side, remote);
            await CloseQuietlyAsync(socket, WebSocketCloseStatus.PolicyViolation, "unknown resumable session");
            return;
        }
        _log.LogInformation("resume attachment waiting room={Room} session={Session} side={Side} remote={Remote}", roomId, sessionId, side, remote);
        var attached = await session.AttachAsync(side, socket, remote, abort);
        _log.LogInformation("resume attachment released room={Room} session={Session} side={Side} remote={Remote} attached={Attached}", roomId, sessionId, side, remote, attached);
        if (!attached)
        {
            await CloseQuietlyAsync(socket, WebSocketCloseStatus.EndpointUnavailable, "resumable session unavailable");
        }
    }

    private ResumeSession NewResumeSession(RelayRoom room, string agentRemote, string clientRemote)
    {
        var session = new ResumeSession(Guid.NewGuid().ToString("N"), room, agentRemote, clientRemote, _log, completed =>
        {
            _sessions.TryRemove($"{room.Id}/{completed.Id}", out _);
        });
        _sessions[$"{room.Id}/{session.Id}"] = session;
        return session;
    }

    public async Task ServeHomeAgentAsync(string token, WebSocket socket, string remote, CancellationToken abort)
    {
        var room = RoomFor(token);
        var stopwatch = Stopwatch.StartNew();
        room.HomeAgentConnected(remote);
        _log.LogInformation("home app connected room={Room} remote={Remote}", room.Id, remote);
        NotifyDashboards();
        try
        {
            var end = await DrainUntilCloseAsync(socket, abort);
            _log.LogInformation("home app receive ended room={Room} remote={Remote} duration_ms={DurationMs} end={End} close_status={CloseStatus} close_reason={CloseReason} error={Error} socket_state={SocketState} request_aborted={RequestAborted}", room.Id, remote, stopwatch.ElapsedMilliseconds, end.End, end.CloseStatus, end.CloseReason, end.Error, socket.State, abort.IsCancellationRequested);
        }
        finally
        {
            room.HomeAgentDisconnected(remote);
            NotifyDashboards();
            _log.LogInformation("home app disconnected room={Room} remote={Remote}", room.Id, remote);
        }
    }

    public async Task ServeDashboardAsync(WebSocket socket, string remote, string? roomId, CancellationToken abort)
    {
        var client = new DashboardClient(Guid.NewGuid(), socket, roomId);
        _dashboards[client.Id] = client;
        _log.LogInformation("dashboard connected remote={Remote}", remote);
        try
        {
            await SendDashboardAsync(client, abort);
            _ = await DrainUntilCloseAsync(socket, abort);
        }
        finally
        {
            _dashboards.TryRemove(client.Id, out _);
            await CloseQuietlyAsync(socket);
            _log.LogInformation("dashboard disconnected remote={Remote}", remote);
        }
    }

    public object Snapshot(string? roomId = null)
    {
        var id = string.IsNullOrWhiteSpace(roomId) ? null : RoomId(roomId);
        var rooms = id is null
            ? _rooms.Values.OrderBy(room => room.Id).Select(room => room.Snapshot()).ToArray()
            : _rooms.TryGetValue(id, out var room) ? new[] { room.Snapshot() } : [];
        return new
        {
            service = "DeskFerry.Relay",
            time = DateTimeOffset.UtcNow,
            rooms
        };
    }

    private RelayRoom RoomFor(string token)
    {
        var id = RoomId(token);
        return _rooms.GetOrAdd(id, key => new RelayRoom(key, _log));
    }

    private static string RoomId(string token)
    {
        var raw = token.Trim().Trim('/');
        if (raw.Length == 0)
        {
            return "default";
        }

        var builder = new StringBuilder(Math.Min(raw.Length, 64));
        foreach (var c in raw)
        {
            if (builder.Length >= 64)
            {
                break;
            }
            if (c is >= 'A' and <= 'Z')
            {
                builder.Append((char)(c + 32));
            }
            else if (c is >= 'a' and <= 'z' or >= '0' and <= '9' or '-' or '_' or '.')
            {
                builder.Append(c);
            }
            else if (builder.Length == 0 || builder[^1] != '-')
            {
                builder.Append('-');
            }
        }

        var room = builder.ToString().Trim('-', '.');
        return room.Length == 0 ? "default" : room;
    }

    private static async Task SendStartAsync(WebSocket socket, CancellationToken cancellationToken)
    {
        await SendControlAsync(socket, "start", cancellationToken);
    }

    private static async Task SendControlAsync(WebSocket socket, string message, CancellationToken cancellationToken)
    {
        var payload = Encoding.UTF8.GetBytes(message);
        await socket.SendAsync(payload, WebSocketMessageType.Text, true, cancellationToken);
    }

    private async Task<bool> TrySendStartAsync(WebSocket socket, string room, string remote, string side, CancellationToken cancellationToken)
    {
        return await TrySendControlAsync(socket, room, remote, side, "start", cancellationToken);
    }

    private async Task<bool> TrySendControlAsync(WebSocket socket, string room, string remote, string side, string message, CancellationToken cancellationToken)
    {
        try
        {
            await SendControlAsync(socket, message, cancellationToken);
            return true;
        }
        catch (OperationCanceledException)
        {
            throw;
        }
        catch (Exception ex)
        {
            _log.LogInformation(ex, "control frame failed room={Room} side={Side} remote={Remote} message={Message}", room, side, remote, message);
            await CloseQuietlyAsync(socket);
            return false;
        }
    }

    private static string CleanSessionValue(string? value)
    {
        value = value?.Trim();
        return value is { Length: 32 } && value.All(Uri.IsHexDigit) ? value.ToLowerInvariant() : "";
    }

    private static async Task<SocketEnd> DrainUntilCloseAsync(WebSocket socket, CancellationToken cancellationToken)
    {
        var buffer = ArrayPool<byte>.Shared.Rent(1024);
        try
        {
            while (socket.State == WebSocketState.Open && !cancellationToken.IsCancellationRequested)
            {
                var result = await socket.ReceiveAsync(buffer, cancellationToken);
                if (result.MessageType == WebSocketMessageType.Close)
                {
                    await socket.CloseAsync(WebSocketCloseStatus.NormalClosure, "", CancellationToken.None);
                    return new SocketEnd("close-frame", result.CloseStatus, result.CloseStatusDescription, null);
                }
            }

            return new SocketEnd(cancellationToken.IsCancellationRequested ? "canceled" : $"socket-{socket.State}", socket.CloseStatus, socket.CloseStatusDescription, null);
        }
        catch (Exception ex)
        {
            return new SocketEnd(ex is OperationCanceledException ? "canceled" : "receive-error", socket.CloseStatus, socket.CloseStatusDescription, ex.ToString());
        }
        finally
        {
            ArrayPool<byte>.Shared.Return(buffer);
        }
    }

    private void NotifyDashboards()
    {
        foreach (var client in _dashboards.Values)
        {
            _ = SendDashboardAsync(client, CancellationToken.None);
        }
    }

    private async Task SendDashboardAsync(DashboardClient client, CancellationToken cancellationToken)
    {
        if (client.Socket.State != WebSocketState.Open)
        {
            _dashboards.TryRemove(client.Id, out _);
            return;
        }
        using var timeout = cancellationToken.CanBeCanceled
            ? CancellationTokenSource.CreateLinkedTokenSource(cancellationToken)
            : new CancellationTokenSource();
        timeout.CancelAfter(TimeSpan.FromSeconds(10));

        await client.Lock.WaitAsync(timeout.Token);
        try
        {
            if (client.Socket.State != WebSocketState.Open)
            {
                _dashboards.TryRemove(client.Id, out _);
                return;
            }
            var payload = Encoding.UTF8.GetBytes(System.Text.Json.JsonSerializer.Serialize(Snapshot(client.RoomId)));
            await client.Socket.SendAsync(payload, WebSocketMessageType.Text, true, timeout.Token);
        }
        catch
        {
            _dashboards.TryRemove(client.Id, out _);
        }
        finally
        {
            client.Lock.Release();
        }
    }

    private static async Task CloseQuietlyAsync(WebSocket socket, WebSocketCloseStatus status = WebSocketCloseStatus.NormalClosure, string reason = "")
    {
        try
        {
            if (socket.State is WebSocketState.Open or WebSocketState.CloseReceived)
            {
                using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(2));
                await socket.CloseAsync(status, reason, timeout.Token);
            }
        }
        catch
        {
            socket.Abort();
        }
    }
}

sealed class RelayRoom
{
    private readonly object _gate = new();
    private readonly Queue<WaitingAgent> _agents = new();
    private readonly ILogger _log;
    private int _activePairs;
    private long _totalPairs;
    private string? _lastAgentRemote;
    private DateTimeOffset? _lastAgentConnectedAt;
    private DateTimeOffset? _lastAgentDisconnectedAt;
    private string? _homeAgentRemote;
    private DateTimeOffset? _homeAgentConnectedAt;
    private DateTimeOffset? _homeAgentDisconnectedAt;
    private string? _lastClientRemote;
    private DateTimeOffset? _lastClientConnectedAt;
    private DateTimeOffset? _lastClientDisconnectedAt;

    public RelayRoom(string id, ILogger log)
    {
        Id = id;
        _log = log;
    }

    public string Id { get; }

    public (WaitingAgent Agent, int Replaced) EnqueueAgent(WebSocket socket, string remote, AgentIdentity identity, bool resumable)
    {
        var agent = new WaitingAgent(socket, remote, identity, resumable);
        List<WaitingAgent> replaced = [];
        lock (_gate)
        {
            PruneClosedAgents();
            if (identity.IsValid)
            {
                var count = _agents.Count;
                for (var i = 0; i < count; i++)
                {
                    var existing = _agents.Dequeue();
                    if (existing.Identity == identity)
                    {
                        existing.TryCancel();
                        replaced.Add(existing);
                    }
                    else
                    {
                        _agents.Enqueue(existing);
                    }
                }
            }
            _agents.Enqueue(agent);
            _lastAgentRemote = remote;
            _lastAgentConnectedAt = DateTimeOffset.UtcNow;
        }
        foreach (var existing in replaced)
        {
            _ = CloseQuietlyAsync(existing.Socket);
        }
        return (agent, replaced.Count);
    }

    public WaitingAgent? TryTakeAgent()
    {
        lock (_gate)
        {
            PruneClosedAgents();
            while (_agents.Count > 0)
            {
                var agent = _agents.Dequeue();
                if (agent.IsOpen)
                {
                    return agent;
                }
            }
            return null;
        }
    }

    public void RemoveWaiting(WaitingAgent waiting)
    {
        lock (_gate)
        {
            var count = _agents.Count;
            for (var i = 0; i < count; i++)
            {
                var agent = _agents.Dequeue();
                if (!ReferenceEquals(agent, waiting))
                {
                    _agents.Enqueue(agent);
                }
            }
            _lastAgentDisconnectedAt = DateTimeOffset.UtcNow;
        }
    }

    public void HomeAgentConnected(string remote)
    {
        lock (_gate)
        {
            _homeAgentRemote = remote;
            _homeAgentConnectedAt = DateTimeOffset.UtcNow;
        }
    }

    public void HomeAgentDisconnected(string remote)
    {
        lock (_gate)
        {
            if (_homeAgentRemote == remote)
            {
                _homeAgentDisconnectedAt = DateTimeOffset.UtcNow;
                _homeAgentRemote = null;
                _homeAgentConnectedAt = null;
            }
        }
    }

    public async Task BridgeAsync(WebSocket agent, WebSocket client, string agentRemote, string clientRemote, TaskCompletionSource clientDone, Action stateChanged, CancellationToken abort)
    {
        var stopwatch = Stopwatch.StartNew();
        var pairId = PairStarted(clientRemote);
        stateChanged();
        try
        {
            using var cts = CancellationTokenSource.CreateLinkedTokenSource(abort);
            var left = PumpAsync(agent, client, "agent_to_client", cts.Token);
            var right = PumpAsync(client, agent, "client_to_agent", cts.Token);
            var firstTask = await Task.WhenAny(left, right);
            var first = await firstTask;
            cts.Cancel();
            var second = await (ReferenceEquals(firstTask, left) ? right : left);
            _log.LogInformation("bridge pumps ended room={Room} pair={PairId} duration_ms={DurationMs} trigger_direction={TriggerDirection} trigger_bytes={TriggerBytes} trigger_messages={TriggerMessages} trigger_end={TriggerEnd} trigger_close_status={TriggerCloseStatus} trigger_close_reason={TriggerCloseReason} trigger_error={TriggerError} other_direction={OtherDirection} other_bytes={OtherBytes} other_messages={OtherMessages} other_end={OtherEnd} other_close_status={OtherCloseStatus} other_close_reason={OtherCloseReason} other_error={OtherError} agent_state={AgentState} client_state={ClientState} request_aborted={RequestAborted}", Id, pairId, stopwatch.ElapsedMilliseconds, first.Direction, first.Bytes, first.Messages, first.End, first.CloseStatus, first.CloseReason, first.Error, second.Direction, second.Bytes, second.Messages, second.End, second.CloseStatus, second.CloseReason, second.Error, agent.State, client.State, abort.IsCancellationRequested);
        }
        finally
        {
            PairEnded();
            await CloseQuietlyAsync(agent);
            await CloseQuietlyAsync(client);
            clientDone.TrySetResult();
            stateChanged();
            _log.LogInformation("bridge closed room={Room} pair={PairId} agent={AgentRemote} client={ClientRemote} duration_ms={DurationMs}", Id, pairId, agentRemote, clientRemote, stopwatch.ElapsedMilliseconds);
        }
    }

    public long PairStarted(string clientRemote)
    {
        lock (_gate)
        {
            _activePairs++;
            _totalPairs++;
            _lastClientRemote = clientRemote;
            _lastClientConnectedAt = DateTimeOffset.UtcNow;
            _lastClientDisconnectedAt = null;
            return _totalPairs;
        }
    }

    public void PairEnded()
    {
        lock (_gate)
        {
            if (_activePairs > 0)
            {
                _activePairs--;
            }
            _lastAgentDisconnectedAt = DateTimeOffset.UtcNow;
            _lastClientDisconnectedAt = DateTimeOffset.UtcNow;
        }
    }

    public object Snapshot()
    {
        lock (_gate)
        {
            PruneClosedAgents();
            return new
            {
                id = Id,
                waiting_agents = _agents.Count,
                active_pairs = _activePairs,
                total_pairs = _totalPairs,
                last_agent_remote = _lastAgentRemote,
                last_agent_connected_at = _lastAgentConnectedAt,
                last_agent_disconnected_at = _lastAgentDisconnectedAt,
                home_agent_connected = _homeAgentRemote is not null,
                home_agent_remote = _homeAgentRemote,
                home_agent_connected_at = _homeAgentConnectedAt,
                home_agent_disconnected_at = _homeAgentDisconnectedAt,
                last_client_remote = _lastClientRemote,
                last_client_connected_at = _lastClientConnectedAt,
                last_client_disconnected_at = _lastClientDisconnectedAt
            };
        }
    }

    private void PruneClosedAgents()
    {
        var count = _agents.Count;
        for (var i = 0; i < count; i++)
        {
            var agent = _agents.Dequeue();
            if (agent.IsOpen)
            {
                _agents.Enqueue(agent);
            }
        }
    }

    internal static async Task<PumpResult> PumpAsync(WebSocket source, WebSocket destination, string direction, CancellationToken cancellationToken)
    {
        var buffer = ArrayPool<byte>.Shared.Rent(64 * 1024);
        long bytes = 0;
        long messages = 0;
        try
        {
            while (source.State == WebSocketState.Open && destination.State == WebSocketState.Open && !cancellationToken.IsCancellationRequested)
            {
                var result = await source.ReceiveAsync(buffer, cancellationToken);
                if (result.MessageType == WebSocketMessageType.Close)
                {
                    return new PumpResult(direction, bytes, messages, "close-frame", result.CloseStatus, result.CloseStatusDescription, null);
                }
                if (result.MessageType != WebSocketMessageType.Binary)
                {
                    continue;
                }
                await destination.SendAsync(new ArraySegment<byte>(buffer, 0, result.Count), WebSocketMessageType.Binary, result.EndOfMessage, cancellationToken);
                bytes += result.Count;
                messages++;
            }

            return new PumpResult(direction, bytes, messages, cancellationToken.IsCancellationRequested ? "canceled" : $"socket-state source={source.State} destination={destination.State}", source.CloseStatus, source.CloseStatusDescription, null);
        }
        catch (Exception ex)
        {
            return new PumpResult(direction, bytes, messages, ex is OperationCanceledException ? "canceled" : "error", source.CloseStatus, source.CloseStatusDescription, ex.ToString());
        }
        finally
        {
            ArrayPool<byte>.Shared.Return(buffer);
        }
    }

    internal static async Task CloseQuietlyAsync(WebSocket socket, WebSocketCloseStatus status = WebSocketCloseStatus.NormalClosure, string reason = "")
    {
        try
        {
            if (socket.State is WebSocketState.Open or WebSocketState.CloseReceived)
            {
                using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(2));
                await socket.CloseAsync(status, reason, timeout.Token);
            }
        }
        catch
        {
            socket.Abort();
        }
    }
}

sealed record SocketEnd(string End, WebSocketCloseStatus? CloseStatus, string? CloseReason, string? Error);

sealed record PumpResult(string Direction, long Bytes, long Messages, string End, WebSocketCloseStatus? CloseStatus, string? CloseReason, string? Error);

sealed record ResumeAttachment(WebSocket Socket, string Remote, TaskCompletionSource Done);

sealed class ResumeSession
{
    private static readonly WebSocketCloseStatus ResumeCloseStatus = (WebSocketCloseStatus)1012;
    private readonly Channel<ResumeAttachment> _agent = Channel.CreateUnbounded<ResumeAttachment>();
    private readonly Channel<ResumeAttachment> _client = Channel.CreateUnbounded<ResumeAttachment>();
    private readonly CancellationTokenSource _finished = new();
    private readonly ILogger _log;
    private readonly Action<ResumeSession> _onFinish;
    private int _finishStarted;

    public ResumeSession(string id, RelayRoom room, string agentRemote, string clientRemote, ILogger log, Action<ResumeSession> onFinish)
    {
        Id = id;
        Room = room;
        AgentRemote = agentRemote;
        ClientRemote = clientRemote;
        _log = log;
        _onFinish = onFinish;
    }

    public string Id { get; }
    public RelayRoom Room { get; }
    public string AgentRemote { get; }
    public string ClientRemote { get; }

    public async Task<bool> AttachAsync(string side, WebSocket socket, string remote, CancellationToken abort)
    {
        var attachment = new ResumeAttachment(socket, remote, new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously));
        var writer = side == "agent" ? _agent.Writer : _client.Writer;
        try
        {
            await writer.WriteAsync(attachment, abort);
        }
        catch (OperationCanceledException)
        {
            return false;
        }
        await Task.WhenAny(attachment.Done.Task, Task.Delay(Timeout.InfiniteTimeSpan, _finished.Token));
        return true;
    }

    public async Task RunAsync(WebSocket initialAgent, WebSocket initialClient, TaskCompletionSource clientDone, Action stateChanged)
    {
        var stopwatch = Stopwatch.StartNew();
        var pairId = Room.PairStarted(ClientRemote);
        stateChanged();
        WebSocket agent = initialAgent;
        WebSocket client = initialClient;
        ResumeAttachment? agentAttachment = null;
        ResumeAttachment? clientAttachment = null;
        try
        {
            while (true)
            {
                var (first, second) = await BridgeOnceAsync(agent, client);
                if (first.CloseStatus == WebSocketCloseStatus.NormalClosure)
                {
                    await Task.WhenAll(
                        RelayRoom.CloseQuietlyAsync(agent, WebSocketCloseStatus.NormalClosure, "session closed"),
                        RelayRoom.CloseQuietlyAsync(client, WebSocketCloseStatus.NormalClosure, "session closed"));
                    agentAttachment?.Done.TrySetResult();
                    clientAttachment?.Done.TrySetResult();
                    return;
                }

                _log.LogInformation("resumable bridge interrupted room={Room} pair={PairId} session={Session} trigger_direction={TriggerDirection} trigger_error={TriggerError} other_direction={OtherDirection} other_error={OtherError}", Room.Id, pairId, Id, first.Direction, first.Error, second.Direction, second.Error);
                await Task.WhenAll(
                    RelayRoom.CloseQuietlyAsync(agent, ResumeCloseStatus, "resume session"),
                    RelayRoom.CloseQuietlyAsync(client, ResumeCloseStatus, "resume session"));
                agentAttachment?.Done.TrySetResult();
                clientAttachment?.Done.TrySetResult();

                using var timeout = CancellationTokenSource.CreateLinkedTokenSource(_finished.Token);
                timeout.CancelAfter(TimeSpan.FromMinutes(5));
                try
                {
                    agentAttachment = await _agent.Reader.ReadAsync(timeout.Token);
                    clientAttachment = await _client.Reader.ReadAsync(timeout.Token);
                }
                catch (OperationCanceledException)
                {
                    return;
                }
                agent = agentAttachment.Socket;
                client = clientAttachment.Socket;
                if (!await TrySendControlAsync(agent, $"resume {Id}") || !await TrySendControlAsync(client, $"resume {Id}"))
                {
                    await Task.WhenAll(
                        RelayRoom.CloseQuietlyAsync(agent, ResumeCloseStatus, "retry resume"),
                        RelayRoom.CloseQuietlyAsync(client, ResumeCloseStatus, "retry resume"));
                    agentAttachment.Done.TrySetResult();
                    clientAttachment.Done.TrySetResult();
                    continue;
                }
                _log.LogInformation("resumable bridge resumed room={Room} pair={PairId} session={Session} agent={AgentRemote} client={ClientRemote}", Room.Id, pairId, Id, agentAttachment.Remote, clientAttachment.Remote);
            }
        }
        finally
        {
            agentAttachment?.Done.TrySetResult();
            clientAttachment?.Done.TrySetResult();
            Room.PairEnded();
            clientDone.TrySetResult();
            Finish();
            stateChanged();
            _log.LogInformation("resumable bridge closed room={Room} pair={PairId} session={Session} agent={AgentRemote} client={ClientRemote} duration_ms={DurationMs}", Room.Id, pairId, Id, AgentRemote, ClientRemote, stopwatch.ElapsedMilliseconds);
        }
    }

    public void Finish()
    {
        if (Interlocked.Exchange(ref _finishStarted, 1) != 0)
        {
            return;
        }
        _finished.Cancel();
        _agent.Writer.TryComplete();
        _client.Writer.TryComplete();
        _onFinish(this);
    }

    private static async Task<(PumpResult First, PumpResult Second)> BridgeOnceAsync(WebSocket agent, WebSocket client)
    {
        using var cts = new CancellationTokenSource();
        var left = RelayRoom.PumpAsync(agent, client, "agent_to_client", cts.Token);
        var right = RelayRoom.PumpAsync(client, agent, "client_to_agent", cts.Token);
        var firstTask = await Task.WhenAny(left, right);
        var first = await firstTask;
        cts.Cancel();
        var second = await (ReferenceEquals(firstTask, left) ? right : left);
        return (first, second);
    }

    private static async Task<bool> TrySendControlAsync(WebSocket socket, string message)
    {
        try
        {
            using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(10));
            await socket.SendAsync(Encoding.UTF8.GetBytes(message), WebSocketMessageType.Text, true, timeout.Token);
            return true;
        }
        catch
        {
            return false;
        }
    }
}

sealed class WaitingAgent
{
    private readonly TaskCompletionSource<HomePeer> _paired = new(TaskCreationOptions.RunContinuationsAsynchronously);

    public WaitingAgent(WebSocket socket, string remote, AgentIdentity identity, bool resumable)
    {
        Socket = socket;
        Remote = remote;
        Identity = identity;
        Resumable = resumable;
    }

    public WebSocket Socket { get; }
    public string Remote { get; }
    public AgentIdentity Identity { get; }
    public bool Resumable { get; }
    public bool IsOpen => Socket.State == WebSocketState.Open && !_paired.Task.IsCompleted;

    public Task<HomePeer> WaitAsync() => _paired.Task;
    public bool TryPair(HomePeer peer) => _paired.TrySetResult(peer);
    public void TryCancel() => _paired.TrySetCanceled();
}

sealed record AgentIdentity(string Instance, string Slot)
{
    public bool IsValid => Instance.Length > 0 && Slot.Length > 0;
    public string LogString => IsValid ? $"{Instance}/{Slot}" : "legacy";
}

sealed record HomePeer(WebSocket Socket, string Remote, TaskCompletionSource Done, TaskCompletionSource<string> Started, bool Resumable);

sealed record DashboardClient(Guid Id, WebSocket Socket, string? RoomId)
{
    public SemaphoreSlim Lock { get; } = new(1, 1);
}
