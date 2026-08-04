import json
import asyncio

from fastapi.testclient import TestClient
from starlette.websockets import WebSocketState

from app import AgentIdentity, RelayHub, app, room_id


class FakeWebSocket:
    def __init__(self, fail_text: bool = False):
        self.client_state = WebSocketState.CONNECTED
        self.application_state = WebSocketState.CONNECTED
        self.fail_text = fail_text
        self.text_messages = []
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

    async def receive(self):
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


def test_home_agent_status_presence():
    client = TestClient(app)
    headers = {"X-DeskFerry-Role": "home-agent"}

    with client.websocket_connect("/relay/unit-home/ws", headers=headers):
        status = client.get("/relay/status?room=unit-home").json()
        assert status["rooms"][0]["home_agent_connected"] is True

    status = client.get("/relay/status?room=unit-home").json()
    assert status["rooms"][0]["home_agent_connected"] is False


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

        await home._received.put({"type": "websocket.disconnect", "code": 1012, "reason": "test drop"})
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

        await resumed_home._received.put({"type": "websocket.disconnect", "code": 1000, "reason": "closed"})
        await asyncio.wait_for(session_task, timeout=2)
        await asyncio.gather(agent_attach, home_attach)
        status = await room.snapshot()
        assert status["active_pairs"] == 0
        assert status["total_pairs"] == 1
        assert removed == [session]

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
