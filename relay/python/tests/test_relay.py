import json
import asyncio

from fastapi.testclient import TestClient
from starlette.requests import Request
from starlette.websockets import WebSocketState

from app import (
    AgentIdentity,
    HTTP_STREAM_ACK,
    HTTP_STREAM_BINARY,
    HTTP_STREAM_TEXT,
    HTTPStreamFrame,
    HTTPStreamWebSocket,
    RELAY_VERSION,
    RelayHub,
    app,
    room_id,
)


class FakeWebSocket:
    def __init__(self, fail_text: bool = False):
        self.client_state = WebSocketState.CONNECTED
        self.application_state = WebSocketState.CONNECTED
        self.fail_text = fail_text
        self.text_messages = []
        self.json_messages = []
        self.byte_messages = []
        self.closed = False
        self.close_code = None
        self.close_reason = ""
        self._received = asyncio.Queue()

    async def send_text(self, text):
        if self.fail_text:
            raise RuntimeError("stale websocket")
        self.text_messages.append(text)

    async def send_bytes(self, payload):
        self.byte_messages.append(payload)

    async def send_json(self, payload):
        self.json_messages.append(payload)

    async def receive(self):
        return await self._received.get()

    async def receive_json(self):
        return await self._received.get()

    async def close(self, code=1000, reason=""):
        self.closed = True
        self.close_code = code
        self.close_reason = reason
        self.client_state = WebSocketState.DISCONNECTED
        self.application_state = WebSocketState.DISCONNECTED


def test_room_id_matches_dotnet_normalization():
    assert room_id("") == "default"
    assert room_id(" WorkDesk ") == "workdesk"
    assert room_id("/Team Room!!/") == "team-room"
    assert room_id("...") == "default"
    assert room_id("A" * 80) == "a" * 64


def test_health_and_empty_status():
    client = TestClient(app)

    health = client.get("/relay/health")
    assert health.status_code == 200
    assert health.json()["service"] == "DeskFerry.Relay"
    assert health.json()["version"] == RELAY_VERSION

    dashboard = client.get("/relay/")
    assert dashboard.status_code == 200
    assert f"v{RELAY_VERSION}" in dashboard.text

    status = client.get("/relay/status?room=unit-empty")
    assert status.status_code == 200
    body = status.json()
    assert body["service"] == "DeskFerry.Relay"
    assert body["rooms"] == []


def test_icon_endpoint():
    client = TestClient(app)

    response = client.get("/relay/icon.svg")
    assert response.status_code == 200
    assert response.headers["content-type"].startswith("image/svg+xml")
    assert "<svg" in response.text


def test_http_stream_sequence_ack_and_duplicate_suppression():
    async def scenario():
        request = Request({
            "type": "http",
            "method": "GET",
            "path": "/relay/unit/stream/id/down",
            "headers": [],
            "query_string": b"",
            "client": ("127.0.0.1", 12345),
            "server": ("127.0.0.1", 80),
            "scheme": "http",
        })
        stream = HTTPStreamWebSocket(request, "s" * 32)
        await stream.apply(HTTPStreamFrame(HTTP_STREAM_TEXT, 1, b"one"))
        await stream.apply(HTTPStreamFrame(HTTP_STREAM_TEXT, 1, b"one"))
        assert await stream.receive_text() == "one"
        assert stream.incoming.empty()

        await stream.send_bytes(b"two")
        frames, ack = await stream.snapshot(0)
        assert ack == 1
        assert [(frame.kind, frame.sequence, frame.payload) for frame in frames] == [
            (HTTP_STREAM_BINARY, 1, b"two")
        ]
        await stream.apply(HTTPStreamFrame(HTTP_STREAM_ACK, 1))
        frames, _ = await stream.snapshot(0)
        assert frames == []

    asyncio.run(scenario())


def test_home_agent_status_presence():
    client = TestClient(app)
    headers = {"X-DeskFerry-Role": "home-agent"}

    with client.websocket_connect("/relay/unit-home/ws", headers=headers):
        status = client.get("/relay/status?room=unit-home").json()
        assert status["rooms"][0]["home_agent_connected"] is True

    status = client.get("/relay/status?room=unit-home").json()
    assert status["rooms"][0]["home_agent_connected"] is False


def test_diagnostic_log_batch_is_acknowledged():
    client = TestClient(app)
    headers = {
        "X-DeskFerry-Role": "diagnostic-log",
        "X-DeskFerry-Log-Component": "home-agent-test",
        "X-DeskFerry-Log-Instance": "unit",
    }
    with client.websocket_connect("/relay/unit-logs/ws", headers=headers) as logs:
        logs.send_json({"entries": ["queued before connect", "connected"]})
        assert logs.receive_json() == {"accepted": 2}


def test_legacy_role_header_is_still_accepted():
    client = TestClient(app)
    headers = {"X-TunnelDesktop-Role": "probe"}

    with client.websocket_connect("/relay/unit-legacy/ws", headers=headers):
        pass


def test_agent_client_pair_and_bridge_bytes():
    client = TestClient(app)
    agent_headers = {"X-DeskFerry-Role": "agent"}
    client_headers = {"X-DeskFerry-Role": "client"}

    with client.websocket_connect("/relay/unit-bridge/ws", headers=agent_headers) as agent:
        with client.websocket_connect("/relay/unit-bridge/ws", headers=client_headers) as home:
            assert agent.receive_text() == "start"
            assert home.receive_text() == "start"

            home.send_bytes(b"from-home")
            assert agent.receive_bytes() == b"from-home"

            agent.send_bytes(b"from-agent")
            assert home.receive_bytes() == b"from-agent"

            status = client.get("/relay/status?room=unit-bridge").json()
            assert status["rooms"][0]["active_pairs"] == 1
            assert status["rooms"][0]["total_pairs"] == 1


def test_resumable_pair_reattaches_after_websocket_drop():
    from app import RelayRoom, ResumeSession

    async def scenario():
        room = RelayRoom("unit-resume")
        removed = []
        session = ResumeSession("0" * 32, room, "work", "home", removed.append)
        agent = FakeWebSocket()
        home = FakeWebSocket()
        client_done = asyncio.Future()
        session_task = asyncio.create_task(session.run(agent, home, client_done, lambda: None))

        await home._received.put({"type": "websocket.receive", "bytes": b"before-drop"})
        for _ in range(50):
            if agent.byte_messages:
                break
            await asyncio.sleep(0.01)
        assert agent.byte_messages == [b"before-drop"]

        # Some proxies terminate a transport with a normal close code but no
        # DeskFerry logical-close marker. The logical session must still resume.
        await home._received.put({"type": "websocket.disconnect", "code": 1000, "reason": ""})
        resumed_agent = FakeWebSocket()
        resumed_home = FakeWebSocket()
        agent_attach = asyncio.create_task(session.attach("agent", resumed_agent, "work-2"))
        home_attach = asyncio.create_task(session.attach("client", resumed_home, "home-2"))
        for _ in range(50):
            if resumed_agent.text_messages and resumed_home.text_messages:
                break
            await asyncio.sleep(0.01)
        assert resumed_agent.text_messages == ["resume " + session.id]
        assert resumed_home.text_messages == ["resume " + session.id]

        await resumed_agent._received.put({"type": "websocket.receive", "bytes": b"after-resume"})
        for _ in range(50):
            if resumed_home.byte_messages:
                break
            await asyncio.sleep(0.01)
        assert resumed_home.byte_messages == [b"after-resume"]

        await resumed_home._received.put({"type": "websocket.disconnect", "code": 1000, "reason": "session closed"})
        await asyncio.wait_for(session_task, timeout=2)
        await asyncio.gather(agent_attach, home_attach)
        status = await room.snapshot()
        assert status["active_pairs"] == 0
        assert status["total_pairs"] == 1
        assert removed == [session]

    asyncio.run(scenario())


def test_resumable_pair_reconstructs_after_relay_restart():
    async def scenario():
        hub = RelayHub()
        session_id = "a" * 32
        proof = "p" * 43
        agent = FakeWebSocket()
        home = FakeWebSocket()
        early_home = FakeWebSocket()
        await hub.serve_resume("unit-restart", early_home, "home-early", session_id, "client", proof, "rdp")
        assert early_home.close_code == 1013
        assert early_home.close_reason == "resume room not ready"

        agent_task = asyncio.create_task(
            hub.serve_resume("unit-restart", agent, "work", session_id, "agent", proof, "rdp")
        )
        home_task = asyncio.create_task(
            hub.serve_resume("unit-restart", home, "home", session_id, "client", proof, "rdp")
        )
        for _ in range(50):
            if agent.text_messages and home.text_messages:
                break
            await asyncio.sleep(0.01)
        assert agent.text_messages == ["resume " + session_id]
        assert home.text_messages == ["resume " + session_id]

        await home._received.put({"type": "websocket.receive", "bytes": b"after-relay-restart"})
        for _ in range(50):
            if agent.byte_messages:
                break
            await asyncio.sleep(0.01)
        assert agent.byte_messages == [b"after-relay-restart"]

        await home._received.put({"type": "websocket.disconnect", "code": 1000, "reason": "session closed"})
        await asyncio.wait_for(asyncio.gather(agent_task, home_task), timeout=2)
        assert f"unit-restart/{session_id}" not in hub._sessions
        status = await hub.snapshot("unit-restart")
        assert status["rooms"][0]["active_pairs"] == 0
        assert status["rooms"][0]["total_pairs"] == 1

    asyncio.run(scenario())


def test_completed_resumable_session_is_not_reconstructed():
    async def scenario():
        hub = RelayHub()
        room = await hub._room_for("unit-completed")
        proof = "p" * 43
        assert await room.authorize_agent(proof)
        session_id = "c" * 32
        session = hub._new_resume_session(room, "work", "home", proof, "rdp", session_id)
        session.finish()

        home = FakeWebSocket()
        await hub.serve_resume("unit-completed", home, "home-2", session_id, "client", proof, "rdp")
        assert home.close_code == 1008
        assert home.close_reason == "unknown resumable session"
        assert f"unit-completed/{session_id}" not in hub._sessions

    asyncio.run(scenario())


def test_dashboard_websocket_receives_snapshot():
    client = TestClient(app)

    with client.websocket_connect("/relay/unit-dashboard/ws?role=dashboard") as dashboard:
        payload = json.loads(dashboard.receive_text())
        assert payload["service"] == "DeskFerry.Relay"
        assert payload["rooms"] == []


def test_client_skips_stale_waiting_agent():
    async def scenario():
        hub = RelayHub()
        stale_agent = FakeWebSocket(fail_text=True)
        live_agent = FakeWebSocket()
        home = FakeWebSocket()

        stale_task = asyncio.create_task(hub.serve_agent("unit-stale", stale_agent, "stale-work"))
        live_task = asyncio.create_task(hub.serve_agent("unit-stale", live_agent, "live-work"))
        await asyncio.sleep(0)

        client_task = asyncio.create_task(hub.serve_client("unit-stale", home, "home"))
        for _ in range(50):
            if home.text_messages:
                break
            await asyncio.sleep(0.01)

        assert stale_agent.closed is True
        assert stale_agent.text_messages == []
        assert live_agent.text_messages == ["start"]
        assert home.text_messages == ["start"]

        for task in (client_task, stale_task, live_task):
            task.cancel()
        await asyncio.gather(client_task, stale_task, live_task, return_exceptions=True)

    asyncio.run(scenario())


def test_agent_identity_replaces_existing_waiting_socket():
    async def scenario():
        hub = RelayHub()
        first = FakeWebSocket()
        second = FakeWebSocket()
        identity = AgentIdentity("unit-agent", "2")

        first_task = asyncio.create_task(hub.serve_agent("unit-replace", first, "work-1", identity))
        await asyncio.sleep(0)
        assert (await hub.snapshot("unit-replace"))["rooms"][0]["waiting_agents"] == 1

        second_task = asyncio.create_task(hub.serve_agent("unit-replace", second, "work-2", identity))
        for _ in range(50):
            status = await hub.snapshot("unit-replace")
            if status["rooms"][0]["waiting_agents"] == 1 and first.closed:
                break
            await asyncio.sleep(0.01)

        status = await hub.snapshot("unit-replace")
        assert status["rooms"][0]["waiting_agents"] == 1
        assert first.closed is True

        for task in (first_task, second_task):
            task.cancel()
        await asyncio.gather(first_task, second_task, return_exceptions=True)

    asyncio.run(scenario())


def test_room_password_rejects_wrong_client_proof():
    async def scenario():
        hub = RelayHub()
        agent = FakeWebSocket()
        wrong_home = FakeWebSocket()

        agent_task = asyncio.create_task(
            hub.serve_agent("unit-protected", agent, "work", proof="correct-proof")
        )
        await asyncio.sleep(0)
        await hub.serve_client("unit-protected", wrong_home, "home", proof="wrong-proof")

        assert wrong_home.closed is True
        assert wrong_home.close_code == 1008
        assert wrong_home.close_reason == "room authentication failed"
        snapshot = await hub.snapshot("unit-protected")
        assert snapshot["rooms"][0]["protected"] is True

        agent_task.cancel()
        await asyncio.gather(agent_task, return_exceptions=True)

    asyncio.run(scenario())


def test_service_channels_do_not_cross_pair():
    async def scenario():
        hub = RelayHub()
        agent = FakeWebSocket()
        rdp_home = FakeWebSocket()
        winrm_home = FakeWebSocket()

        agent_task = asyncio.create_task(
            hub.serve_agent("unit-services", agent, "work", proof="proof", service="winrm")
        )
        await asyncio.sleep(0)
        await hub.serve_client("unit-services", rdp_home, "rdp-home", proof="proof", service="rdp")
        assert rdp_home.close_code == 1013

        client_task = asyncio.create_task(
            hub.serve_client("unit-services", winrm_home, "winrm-home", proof="proof", service="winrm")
        )
        for _ in range(50):
            if winrm_home.text_messages:
                break
            await asyncio.sleep(0.01)
        assert agent.text_messages == ["start"]
        assert winrm_home.text_messages == ["start"]

        for task in (agent_task, client_task):
            task.cancel()
        await asyncio.gather(agent_task, client_task, return_exceptions=True)

    asyncio.run(scenario())


def test_smb_service_channel_pairs_only_smb():
    async def scenario():
        hub = RelayHub()
        agent = FakeWebSocket()
        winrm_home = FakeWebSocket()
        smb_home = FakeWebSocket()

        agent_task = asyncio.create_task(
            hub.serve_agent("unit-smb", agent, "work", proof="proof", service="smb")
        )
        await asyncio.sleep(0)
        await hub.serve_client("unit-smb", winrm_home, "winrm-home", proof="proof", service="winrm")
        assert winrm_home.close_code == 1013

        client_task = asyncio.create_task(
            hub.serve_client("unit-smb", smb_home, "smb-home", proof="proof", service="smb")
        )
        for _ in range(50):
            if smb_home.text_messages:
                break
            await asyncio.sleep(0.01)
        assert agent.text_messages == ["start"]
        assert smb_home.text_messages == ["start"]

        for task in (agent_task, client_task):
            task.cancel()
        await asyncio.gather(agent_task, client_task, return_exceptions=True)

    asyncio.run(scenario())


def test_v2_on_demand_pairing_and_busy_rejection():
    async def scenario():
        hub = RelayHub()
        control = FakeWebSocket()
        home = FakeWebSocket()
        agent = FakeWebSocket()
        control_task = asyncio.create_task(
            hub.serve_agent_control("unit-v2", control, "work", "unit-agent", {"screen"}, 1)
        )
        for _ in range(50):
            if control.json_messages:
                break
            await asyncio.sleep(0.01)
        assert control.json_messages[0]["type"] == "control-ready"

        home_task = asyncio.create_task(
            hub.serve_v2_client("unit-v2", home, "home", True, "", "screen", heartbeat=True)
        )
        for _ in range(50):
            if len(control.json_messages) > 1:
                break
            await asyncio.sleep(0.01)
        offer = control.json_messages[1]
        assert offer["type"] == "session-offer"
        assert offer["service"] == "screen"
        assert offer["heartbeat"] is True
        await control._received.put({"type": "accept", "session_id": offer["session_id"], "heartbeat": True})
        agent_task = asyncio.create_task(
            hub.serve_agent_session(
                "unit-v2", agent, "work-data", "unit-agent",
                offer["session_id"], True, "", "screen",
            )
        )
        for _ in range(50):
            if home.json_messages and agent.json_messages:
                break
            await asyncio.sleep(0.01)
        assert home.json_messages[-1]["type"] == "session-ready"
        assert home.json_messages[-1]["service"] == "screen"
        assert home.json_messages[-1]["heartbeat"] is True
        assert agent.json_messages[-1]["session_id"] == offer["session_id"]
        assert agent.json_messages[-1]["service"] == "screen"
        assert agent.json_messages[-1]["heartbeat"] is True

        busy = FakeWebSocket()
        await hub.serve_v2_client("unit-v2", busy, "home-2", True, "", "screen")
        assert busy.json_messages[-1]["type"] == "busy"
        status = (await hub.snapshot("unit-v2"))["rooms"][0]
        assert status["control_connections"] == 1
        assert status["pending_requests"] == 0
        assert status["busy_rejections"] == 1

        await home._received.put({"type": "websocket.receive", "bytes": b"v2-data"})
        for _ in range(50):
            if agent.byte_messages:
                break
            await asyncio.sleep(0.01)
        assert agent.byte_messages == [b"v2-data"]

        for task in (home_task, agent_task, control_task):
            task.cancel()
        await asyncio.gather(home_task, agent_task, control_task, return_exceptions=True)

    asyncio.run(scenario())


def test_v2_no_agent_is_typed_and_immediate():
    async def scenario():
        hub = RelayHub()
        home = FakeWebSocket()
        started = time.monotonic()
        await hub.serve_v2_client("unit-no-agent", home, "home")
        assert time.monotonic() - started < 0.5
        assert home.json_messages[-1]["type"] == "no-agent"
        assert home.close_code == 1000

    import time
    asyncio.run(scenario())


def test_legacy_client_is_translated_to_v2_control_offer():
    async def scenario():
        hub = RelayHub()
        control = FakeWebSocket()
        legacy_home = FakeWebSocket()
        agent = FakeWebSocket()
        control_task = asyncio.create_task(
            hub.serve_agent_control("unit-mixed", control, "work", "unit-agent", {"smb"}, 1)
        )
        for _ in range(50):
            if control.json_messages:
                break
            await asyncio.sleep(0.01)
        home_task = asyncio.create_task(
            hub.serve_client("unit-mixed", legacy_home, "old-home", False, "", "smb")
        )
        for _ in range(50):
            if len(control.json_messages) > 1:
                break
            await asyncio.sleep(0.01)
        offer = control.json_messages[1]
        assert offer["type"] == "session-offer"
        assert offer["resumable"] is False
        await control._received.put({"type": "accept", "session_id": offer["session_id"]})
        agent_task = asyncio.create_task(
            hub.serve_agent_session("unit-mixed", agent, "work-data", "unit-agent", offer["session_id"], False, "", "smb")
        )
        for _ in range(50):
            if legacy_home.text_messages and agent.json_messages:
                break
            await asyncio.sleep(0.01)
        assert legacy_home.text_messages == ["start " + offer["session_id"]]
        assert agent.json_messages[-1]["type"] == "session-ready"
        await legacy_home._received.put({"type": "websocket.receive", "bytes": b"legacy-smb"})
        for _ in range(50):
            if agent.byte_messages:
                break
            await asyncio.sleep(0.01)
        assert agent.byte_messages == [b"legacy-smb"]
        for task in (home_task, agent_task, control_task):
            task.cancel()
        await asyncio.gather(home_task, agent_task, control_task, return_exceptions=True)

    asyncio.run(scenario())
