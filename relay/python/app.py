from __future__ import annotations

import asyncio
import hmac
import json
import logging
import time
import uuid
from collections import deque
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

from fastapi import FastAPI, WebSocket
from fastapi.responses import HTMLResponse, JSONResponse, RedirectResponse, Response
from starlette.websockets import WebSocketDisconnect, WebSocketState

SERVICE_NAME = "DeskFerry.Relay"
DASHBOARD_ROLE = "dashboard"
RESUME_ROLE = "resume"
STARTED = "started"
AGENT_UNAVAILABLE = "agent-unavailable"
CLIENT_UNAVAILABLE = "client-unavailable"
VALID_ROLES = {"agent", "client", "home-agent", "probe", RESUME_ROLE, DASHBOARD_ROLE}

logger = logging.getLogger("deskferry.relay")
logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")

app = FastAPI(title="DeskFerry Python Relay", docs_url=None, redoc_url=None)


def utc_now() -> datetime:
    return datetime.now(timezone.utc)


def json_time(value: datetime | None) -> str | None:
    if value is None:
        return None
    return value.isoformat().replace("+00:00", "Z")


def room_id(token: str | None) -> str:
    raw = (token or "").strip().strip("/")
    if not raw:
        return "default"

    out: list[str] = []
    for ch in raw:
        if len(out) >= 64:
            break
        lower = ch.lower()
        if "a" <= lower <= "z" or "0" <= lower <= "9" or lower in "-_.":
            out.append(lower)
        elif not out or out[-1] != "-":
            out.append("-")

    normalized = "".join(out).strip("-.")
    return normalized or "default"


def websocket_is_connected(websocket: WebSocket) -> bool:
    return (
        websocket.client_state == WebSocketState.CONNECTED
        and websocket.application_state == WebSocketState.CONNECTED
    )


def websocket_remote(websocket: WebSocket) -> str:
    forwarded = websocket.headers.get("x-forwarded-for", "")
    if forwarded.strip():
        return forwarded.split(",", 1)[0].strip()
    return websocket.client.host if websocket.client else "unknown"


def try_set_result(future: asyncio.Future[Any], value: Any) -> None:
    if not future.done():
        future.set_result(value)


def try_cancel(future: asyncio.Future[Any]) -> None:
    if not future.done():
        future.cancel()


def read_role(websocket: WebSocket) -> str | None:
    value = (
        websocket.headers.get("x-deskferry-role")
        or websocket.headers.get("x-tunneldesktop-role")
        or websocket.query_params.get("role")
    )
    role = (value or "").strip().lower()
    return role if role in VALID_ROLES else None


def read_token(websocket: WebSocket) -> str | None:
    auth = websocket.headers.get("authorization", "")
    if auth.lower().startswith("bearer "):
        token = auth[7:].strip()
        if token:
            return token

    token = (websocket.query_params.get("token") or "").strip()
    if token:
        return token

    room = (websocket.query_params.get("room") or "").strip()
    return room or "default"


def clean_agent_identity(value: str | None) -> str:
    raw = (value or "").strip()
    out: list[str] = []
    for ch in raw:
        if len(out) >= 64:
            break
        if "a" <= ch <= "z" or "A" <= ch <= "Z" or "0" <= ch <= "9" or ch in "-_.":
            out.append(ch)
    return "".join(out)


def read_agent_identity(websocket: WebSocket) -> AgentIdentity:
    return AgentIdentity(
        clean_agent_identity(websocket.headers.get("x-deskferry-agent-instance")),
        clean_agent_identity(websocket.headers.get("x-deskferry-agent-slot")),
        read_service(websocket),
    )


def read_resumable(websocket: WebSocket) -> bool:
    return websocket.headers.get("x-deskferry-resumable", "").strip().lower() in {"1", "true"}


def read_room_proof(websocket: WebSocket) -> str:
    return websocket.headers.get("x-deskferry-room-proof", "").strip()


def read_service(websocket: WebSocket) -> str:
    return "winrm" if websocket.headers.get("x-deskferry-service", "").strip().lower() == "winrm" else "rdp"


def clean_session_value(value: str | None) -> str:
    value = (value or "").strip().lower()
    return value if len(value) == 32 and all(ch in "0123456789abcdef" for ch in value) else ""


async def close_quietly(websocket: WebSocket, code: int = 1000, reason: str = "") -> None:
    try:
        if websocket.application_state == WebSocketState.CONNECTED:
            await websocket.close(code=code, reason=reason)
    except Exception:
        pass


async def drain_until_close(websocket: WebSocket) -> tuple[str, Any, str | None]:
    try:
        while websocket_is_connected(websocket):
            message = await websocket.receive()
            if message["type"] == "websocket.disconnect":
                return "close-frame", message.get("code"), message.get("reason")
        return "socket-state", None, None
    except WebSocketDisconnect as exc:
        return "disconnect", getattr(exc, "code", None), getattr(exc, "reason", None)
    except asyncio.CancelledError:
        return "canceled", None, None
    except Exception as exc:
        return f"error: {exc!r}", None, None


@dataclass
class PumpResult:
    direction: str
    byte_count: int = 0
    messages: int = 0
    end: str = "socket-state"
    close_code: Any = None
    close_reason: str | None = None
    error: str | None = None


async def pump_binary(source: WebSocket, destination: WebSocket, direction: str) -> PumpResult:
    result = PumpResult(direction)
    try:
        while websocket_is_connected(source) and websocket_is_connected(destination):
            message = await source.receive()
            if message["type"] == "websocket.disconnect":
                result.end = "close-frame"
                result.close_code = message.get("code")
                result.close_reason = message.get("reason")
                return result
            payload = message.get("bytes")
            if payload is not None:
                await destination.send_bytes(payload)
                result.byte_count += len(payload)
                result.messages += 1
        return result
    except asyncio.CancelledError:
        result.end = "canceled"
        return result
    except WebSocketDisconnect as exc:
        result.end = "disconnect"
        result.close_code = getattr(exc, "code", None)
        result.close_reason = getattr(exc, "reason", None)
        return result
    except Exception as exc:
        result.end = "error"
        result.error = repr(exc)
        return result


async def send_start(websocket: WebSocket, side: str, room: str, remote: str) -> bool:
    return await send_control(websocket, "start", side, room, remote)


async def send_control(websocket: WebSocket, message: str, side: str, room: str, remote: str) -> bool:
    try:
        await websocket.send_text(message)
        return True
    except asyncio.CancelledError:
        raise
    except Exception:
        logger.info("control frame failed room=%s side=%s remote=%s message=%r", room, side, remote, message, exc_info=True)
        await close_quietly(websocket)
        return False


@dataclass
class AgentIdentity:
    instance: str = ""
    slot: str = ""
    service: str = "rdp"

    @property
    def is_valid(self) -> bool:
        return bool(self.instance and self.slot)

    @property
    def log_string(self) -> str:
        return f"{self.instance}/{self.service}/{self.slot}" if self.is_valid else "legacy"


@dataclass
class HomePeer:
    websocket: WebSocket
    remote: str
    done: asyncio.Future[None]
    started: asyncio.Future[str]
    resumable: bool = False


@dataclass
class WaitingAgent:
    websocket: WebSocket
    remote: str
    identity: AgentIdentity = field(default_factory=AgentIdentity)
    resumable: bool = False
    service: str = "rdp"
    paired: asyncio.Future[HomePeer] = field(default_factory=asyncio.Future)

    @property
    def is_open(self) -> bool:
        return websocket_is_connected(self.websocket) and not self.paired.done()

    def try_pair(self, peer: HomePeer) -> bool:
        if self.paired.done():
            return False
        self.paired.set_result(peer)
        return True

    def try_cancel(self) -> None:
        if not self.paired.done():
            self.paired.cancel()


class RelayRoom:
    def __init__(self, room: str) -> None:
        self.id = room
        self._lock = asyncio.Lock()
        self._agents: deque[WaitingAgent] = deque()
        self._active_pairs = 0
        self._credential_set = False
        self._room_proof = ""
        self._total_pairs = 0
        self._last_agent_remote: str | None = None
        self._last_agent_connected_at: datetime | None = None
        self._last_agent_disconnected_at: datetime | None = None
        self._home_agent_remote: str | None = None
        self._home_agent_connected_at: datetime | None = None
        self._home_agent_disconnected_at: datetime | None = None
        self._last_client_remote: str | None = None
        self._last_client_connected_at: datetime | None = None
        self._last_client_disconnected_at: datetime | None = None

    async def authorize_agent(self, proof: str) -> bool:
        async with self._lock:
            self._prune_closed_agents_locked()
            if not self._credential_set or (not self._agents and self._active_pairs == 0):
                self._credential_set = True
                self._room_proof = proof
                return True
            return hmac.compare_digest(self._room_proof, proof)

    async def authorize_client(self, proof: str) -> bool:
        async with self._lock:
            return (not self._credential_set and not proof) or (
                self._credential_set and hmac.compare_digest(self._room_proof, proof)
            )

    async def enqueue_agent(self, websocket: WebSocket, remote: str, identity: AgentIdentity, resumable: bool = False, service: str = "rdp") -> tuple[WaitingAgent, int]:
        waiting = WaitingAgent(websocket, remote, identity, resumable, service)
        replaced: list[WaitingAgent] = []
        async with self._lock:
            self._prune_closed_agents_locked()
            if identity.is_valid:
                kept: deque[WaitingAgent] = deque()
                while self._agents:
                    agent = self._agents.popleft()
                    if agent.identity == identity:
                        agent.try_cancel()
                        replaced.append(agent)
                    else:
                        kept.append(agent)
                self._agents = kept
            self._agents.append(waiting)
            self._last_agent_remote = remote
            self._last_agent_connected_at = utc_now()
        for agent in replaced:
            await close_quietly(agent.websocket, reason="replaced by newer agent socket")
        return waiting, len(replaced)

    async def try_take_agent(self, service: str = "rdp") -> WaitingAgent | None:
        async with self._lock:
            self._prune_closed_agents_locked()
            for _ in range(len(self._agents)):
                waiting = self._agents.popleft()
                if waiting.is_open and waiting.service == service:
                    return waiting
                if waiting.is_open:
                    self._agents.append(waiting)
        return None

    async def remove_waiting(self, waiting: WaitingAgent) -> None:
        async with self._lock:
            self._agents = deque(agent for agent in self._agents if agent is not waiting)
            self._last_agent_disconnected_at = utc_now()

    async def home_agent_connected(self, remote: str) -> None:
        async with self._lock:
            self._home_agent_remote = remote
            self._home_agent_connected_at = utc_now()

    async def home_agent_disconnected(self, remote: str) -> None:
        async with self._lock:
            if self._home_agent_remote == remote:
                self._home_agent_remote = None
                self._home_agent_connected_at = None
                self._home_agent_disconnected_at = utc_now()

    async def bridge(
        self,
        agent: WebSocket,
        client: WebSocket,
        agent_remote: str,
        client_remote: str,
        client_done: asyncio.Future[None],
        state_changed: Any,
    ) -> None:
        started = time.monotonic()
        pair_id = await self.pair_started(client_remote)
        state_changed()

        left = asyncio.create_task(pump_binary(agent, client, "agent_to_client"))
        right = asyncio.create_task(pump_binary(client, agent, "client_to_agent"))
        try:
            done, pending = await asyncio.wait({left, right}, return_when=asyncio.FIRST_COMPLETED)
            first_task = next(iter(done))
            first = await first_task
            second_task = right if first_task is left else left
            if not second_task.done():
                second_task.cancel()
            second = await second_task
            logger.info(
                "bridge pumps ended room=%s pair=%s duration_ms=%d trigger_direction=%s trigger_bytes=%d trigger_messages=%d trigger_end=%s trigger_close_code=%s trigger_close_reason=%r trigger_error=%r other_direction=%s other_bytes=%d other_messages=%d other_end=%s other_close_code=%s other_close_reason=%r other_error=%r",
                self.id, pair_id, round((time.monotonic() - started) * 1000), first.direction, first.byte_count, first.messages, first.end, first.close_code, first.close_reason, first.error, second.direction, second.byte_count, second.messages, second.end, second.close_code, second.close_reason, second.error,
            )
        finally:
            await self.pair_ended()
            await close_quietly(agent)
            await close_quietly(client)
            if not client_done.done():
                client_done.set_result(None)
            state_changed()
            logger.info("bridge closed room=%s pair=%s agent=%s client=%s duration_ms=%d", self.id, pair_id, agent_remote, client_remote, round((time.monotonic() - started) * 1000))

    async def pair_started(self, client_remote: str) -> int:
        async with self._lock:
            self._active_pairs += 1
            self._total_pairs += 1
            self._last_client_remote = client_remote
            self._last_client_connected_at = utc_now()
            self._last_client_disconnected_at = None
            return self._total_pairs

    async def pair_ended(self) -> None:
        async with self._lock:
            self._active_pairs = max(0, self._active_pairs - 1)
            self._last_agent_disconnected_at = utc_now()
            self._last_client_disconnected_at = utc_now()

    async def snapshot(self) -> dict[str, Any]:
        async with self._lock:
            self._prune_closed_agents_locked()
            return {
                "id": self.id,
                "protected": self._credential_set and bool(self._room_proof),
                "waiting_agents": len(self._agents),
                "active_pairs": self._active_pairs,
                "total_pairs": self._total_pairs,
                "last_agent_remote": self._last_agent_remote,
                "last_agent_connected_at": json_time(self._last_agent_connected_at),
                "last_agent_disconnected_at": json_time(self._last_agent_disconnected_at),
                "home_agent_connected": self._home_agent_remote is not None,
                "home_agent_remote": self._home_agent_remote,
                "home_agent_connected_at": json_time(self._home_agent_connected_at),
                "home_agent_disconnected_at": json_time(self._home_agent_disconnected_at),
                "last_client_remote": self._last_client_remote,
                "last_client_connected_at": json_time(self._last_client_connected_at),
                "last_client_disconnected_at": json_time(self._last_client_disconnected_at),
            }

    def _prune_closed_agents_locked(self) -> None:
        self._agents = deque(agent for agent in self._agents if agent.is_open)


@dataclass
class ResumeAttachment:
    websocket: WebSocket
    remote: str
    done: asyncio.Future[None]


class ResumeSession:
    def __init__(
        self,
        session_id: str,
        room: RelayRoom,
        agent_remote: str,
        client_remote: str,
        on_finish: Any,
        room_proof: str = "",
        service: str = "rdp",
    ) -> None:
        self.id = session_id
        self.room = room
        self.agent_remote = agent_remote
        self.client_remote = client_remote
        self.room_proof = room_proof
        self.service = service
        self._agent: asyncio.Queue[ResumeAttachment] = asyncio.Queue(maxsize=2)
        self._client: asyncio.Queue[ResumeAttachment] = asyncio.Queue(maxsize=2)
        self._done = asyncio.Event()
        self._on_finish = on_finish

    async def attach(self, side: str, websocket: WebSocket, remote: str) -> bool:
        attachment = ResumeAttachment(websocket, remote, asyncio.Future())
        queue = self._agent if side == "agent" else self._client
        try:
            await queue.put(attachment)
            done_task = asyncio.create_task(self._done.wait())
            await asyncio.wait({attachment.done, done_task}, return_when=asyncio.FIRST_COMPLETED)
            done_task.cancel()
            return True
        except asyncio.CancelledError:
            return False

    async def run(
        self,
        agent: WebSocket,
        client: WebSocket,
        client_done: asyncio.Future[None],
        state_changed: Any,
    ) -> None:
        started_at = time.monotonic()
        pair_id = await self.room.pair_started(self.client_remote)
        state_changed()
        agent_attachment: ResumeAttachment | None = None
        client_attachment: ResumeAttachment | None = None
        try:
            while True:
                first, second = await bridge_once(agent, client)
                if first.close_code == 1000:
                    await close_quietly(agent, 1000, "session closed")
                    await close_quietly(client, 1000, "session closed")
                    if agent_attachment is not None:
                        try_set_result(agent_attachment.done, None)
                    if client_attachment is not None:
                        try_set_result(client_attachment.done, None)
                    return

                logger.info(
                    "resumable bridge interrupted room=%s pair=%s session=%s trigger_direction=%s trigger_error=%r other_direction=%s other_error=%r",
                    self.room.id, pair_id, self.id, first.direction, first.error, second.direction, second.error,
                )
                await close_quietly(agent, 1012, "resume session")
                await close_quietly(client, 1012, "resume session")
                if agent_attachment is not None:
                    try_set_result(agent_attachment.done, None)
                if client_attachment is not None:
                    try_set_result(client_attachment.done, None)
                try:
                    agent_attachment, client_attachment = await asyncio.wait_for(
                        asyncio.gather(self._agent.get(), self._client.get()), timeout=300
                    )
                except (asyncio.TimeoutError, asyncio.CancelledError):
                    return
                agent = agent_attachment.websocket
                client = client_attachment.websocket
                if not await send_control(agent, f"resume {self.id}", "agent", self.room.id, agent_attachment.remote) or not await send_control(client, f"resume {self.id}", "client", self.room.id, client_attachment.remote):
                    await close_quietly(agent, 1012, "retry resume")
                    await close_quietly(client, 1012, "retry resume")
                    try_set_result(agent_attachment.done, None)
                    try_set_result(client_attachment.done, None)
                    continue
                logger.info("resumable bridge resumed room=%s pair=%s session=%s agent=%s client=%s", self.room.id, pair_id, self.id, agent_attachment.remote, client_attachment.remote)
        finally:
            if agent_attachment is not None:
                try_set_result(agent_attachment.done, None)
            if client_attachment is not None:
                try_set_result(client_attachment.done, None)
            await self.room.pair_ended()
            try_set_result(client_done, None)
            self.finish()
            state_changed()
            logger.info("resumable bridge closed room=%s pair=%s session=%s agent=%s client=%s duration_ms=%d", self.room.id, pair_id, self.id, self.agent_remote, self.client_remote, round((time.monotonic() - started_at) * 1000))

    def finish(self) -> None:
        if self._done.is_set():
            return
        self._done.set()
        self._on_finish(self)


async def bridge_once(agent: WebSocket, client: WebSocket) -> tuple[PumpResult, PumpResult]:
    left = asyncio.create_task(pump_binary(agent, client, "agent_to_client"))
    right = asyncio.create_task(pump_binary(client, agent, "client_to_agent"))
    done, _ = await asyncio.wait({left, right}, return_when=asyncio.FIRST_COMPLETED)
    first_task = next(iter(done))
    first = await first_task
    second_task = right if first_task is left else left
    if not second_task.done():
        second_task.cancel()
    second = await second_task
    return first, second


@dataclass
class DashboardClient:
    id: str
    websocket: WebSocket
    room: str | None
    lock: asyncio.Lock = field(default_factory=asyncio.Lock)


class RelayHub:
    def __init__(self) -> None:
        self._lock = asyncio.Lock()
        self._rooms: dict[str, RelayRoom] = {}
        self._dashboards: dict[str, DashboardClient] = {}
        self._sessions: dict[str, ResumeSession] = {}

    async def serve_agent(
        self,
        token: str,
        websocket: WebSocket,
        remote: str,
        identity: AgentIdentity | None = None,
        resumable: bool = False,
        proof: str = "",
        service: str = "rdp",
    ) -> None:
        room = await self._room_for(token)
        if not await room.authorize_agent(proof):
            await close_quietly(websocket, 1008, "room authentication failed")
            return
        identity = identity or AgentIdentity()
        waiting, replaced = await room.enqueue_agent(websocket, remote, identity, resumable, service)
        logger.info("agent waiting room=%s service=%s remote=%s key=%s replaced=%s", room.id, service, remote, identity.log_string, replaced)
        self.notify_dashboards()

        peer: HomePeer | None = None
        try:
            peer = await waiting.paired
            logger.info("pairing room=%s agent=%s client=%s", room.id, remote, peer.remote)
            if waiting.resumable and peer.resumable:
                session = self._new_resume_session(room, remote, peer.remote, proof, service)
                if not await send_control(websocket, f"start {session.id}", "agent", room.id, remote):
                    try_set_result(peer.started, AGENT_UNAVAILABLE)
                    session.finish()
                    return
                if not await send_control(peer.websocket, f"start {session.id}", "client", room.id, peer.remote):
                    try_set_result(peer.started, CLIENT_UNAVAILABLE)
                    try_set_result(peer.done, None)
                    session.finish()
                    return
                try_set_result(peer.started, STARTED)
                await session.run(websocket, peer.websocket, peer.done, self.notify_dashboards)
                return
            if not await send_start(websocket, "agent", room.id, remote):
                try_set_result(peer.started, AGENT_UNAVAILABLE)
                return
            if not await send_start(peer.websocket, "client", room.id, peer.remote):
                try_set_result(peer.started, CLIENT_UNAVAILABLE)
                try_set_result(peer.done, None)
                return
            try_set_result(peer.started, STARTED)
            await room.bridge(websocket, peer.websocket, remote, peer.remote, peer.done, self.notify_dashboards)
        except (asyncio.CancelledError, WebSocketDisconnect):
            if peer is not None and not peer.started.done():
                try_cancel(peer.started)
            pass
        except Exception:
            logger.exception("agent websocket ended room=%s remote=%s", room.id, remote)
            if peer is not None and not peer.started.done():
                try_set_result(peer.started, AGENT_UNAVAILABLE)
        finally:
            if peer is not None and not peer.started.done():
                try_set_result(peer.started, AGENT_UNAVAILABLE)
            waiting.try_cancel()
            await room.remove_waiting(waiting)
            self.notify_dashboards()

    async def serve_client(self, token: str, websocket: WebSocket, remote: str, resumable: bool = False, proof: str = "", service: str = "rdp") -> None:
        room = await self._room_for(token)
        if not await room.authorize_client(proof):
            await close_quietly(websocket, 1008, "room authentication failed")
            return
        while websocket_is_connected(websocket):
            waiting = await room.try_take_agent(service)
            if waiting is None:
                logger.info("client rejected without agent room=%s remote=%s", room.id, remote)
                await close_quietly(websocket, 1013, "no work agent connected")
                return

            done: asyncio.Future[None] = asyncio.Future()
            started: asyncio.Future[str] = asyncio.Future()
            if not waiting.try_pair(HomePeer(websocket, remote, done, started, resumable)):
                continue
            self.notify_dashboards()

            try:
                start_result = await started
                if start_result == STARTED:
                    await done
                    return
                if start_result == CLIENT_UNAVAILABLE:
                    return
                logger.info("skipped unavailable work agent room=%s agent=%s client=%s", room.id, waiting.remote, remote)
            except asyncio.CancelledError:
                try_cancel(done)
                return

        await close_quietly(websocket)

    async def serve_resume(self, token: str, websocket: WebSocket, remote: str, session_id: str | None, side: str | None, proof: str = "", service: str = "rdp") -> None:
        session_id = clean_session_value(session_id)
        side = (side or "").strip().lower()
        session = self._sessions.get(f"{room_id(token)}/{session_id}")
        if (not session_id or side not in {"agent", "client"} or session is None or
                session.service != service or not hmac.compare_digest(session.room_proof, proof)):
            await close_quietly(websocket, 1008, "unknown resumable session")
            return
        if not await session.attach(side, websocket, remote):
            await close_quietly(websocket, 1013, "resumable session unavailable")

    def _new_resume_session(self, room: RelayRoom, agent_remote: str, client_remote: str, proof: str, service: str) -> ResumeSession:
        def remove(session: ResumeSession) -> None:
            self._sessions.pop(f"{room.id}/{session.id}", None)

        session = ResumeSession(uuid.uuid4().hex, room, agent_remote, client_remote, remove, proof, service)
        self._sessions[f"{room.id}/{session.id}"] = session
        return session

    async def serve_home_agent(self, token: str, websocket: WebSocket, remote: str, proof: str = "") -> None:
        room = await self._room_for(token)
        if not await room.authorize_client(proof):
            await close_quietly(websocket, 1008, "room authentication failed")
            return
        started_at = time.monotonic()
        await room.home_agent_connected(remote)
        logger.info("home app connected room=%s remote=%s", room.id, remote)
        self.notify_dashboards()
        try:
            end, close_code, close_reason = await drain_until_close(websocket)
            logger.info("home app receive ended room=%s remote=%s duration_ms=%d end=%s close_code=%s close_reason=%r client_state=%s application_state=%s", room.id, remote, round((time.monotonic() - started_at) * 1000), end, close_code, close_reason, websocket.client_state, websocket.application_state)
        finally:
            await room.home_agent_disconnected(remote)
            self.notify_dashboards()
            logger.info("home app disconnected room=%s remote=%s", room.id, remote)

    async def serve_probe(self, token: str, websocket: WebSocket, proof: str = "") -> None:
        room = await self._room_for(token)
        if not await room.authorize_client(proof):
            await close_quietly(websocket, 1008, "room authentication failed")
            return
        await close_quietly(websocket, 1000, "probe ok")

    async def serve_dashboard(self, websocket: WebSocket, remote: str, room: str | None) -> None:
        client = DashboardClient(str(uuid.uuid4()), websocket, room_id(room) if room else None)
        self._dashboards[client.id] = client
        logger.info("dashboard connected remote=%s", remote)
        try:
            await self._send_dashboard(client)
            await drain_until_close(websocket)
        finally:
            self._dashboards.pop(client.id, None)
            await close_quietly(websocket)
            logger.info("dashboard disconnected remote=%s", remote)

    async def snapshot(self, room: str | None = None) -> dict[str, Any]:
        selected = room_id(room) if room else None
        async with self._lock:
            rooms = list(self._rooms.values())

        if selected is not None:
            rooms = [candidate for candidate in rooms if candidate.id == selected]

        return {
            "service": SERVICE_NAME,
            "time": json_time(utc_now()),
            "rooms": [await candidate.snapshot() for candidate in sorted(rooms, key=lambda item: item.id)],
        }

    def notify_dashboards(self) -> None:
        for client in list(self._dashboards.values()):
            asyncio.create_task(self._send_dashboard(client))

    async def _room_for(self, token: str) -> RelayRoom:
        key = room_id(token)
        async with self._lock:
            if key not in self._rooms:
                self._rooms[key] = RelayRoom(key)
            return self._rooms[key]

    async def _send_dashboard(self, client: DashboardClient) -> None:
        if not websocket_is_connected(client.websocket):
            self._dashboards.pop(client.id, None)
            return

        try:
            async with client.lock:
                if not websocket_is_connected(client.websocket):
                    self._dashboards.pop(client.id, None)
                    return
                payload = json.dumps(await self.snapshot(client.room), separators=(",", ":"))
                await asyncio.wait_for(client.websocket.send_text(payload), timeout=10)
        except Exception:
            self._dashboards.pop(client.id, None)


hub = RelayHub()


@app.get("/", include_in_schema=False)
async def root() -> RedirectResponse:
    return RedirectResponse("/relay/")


@app.get("/relay", response_class=HTMLResponse, include_in_schema=False)
@app.get("/relay/", response_class=HTMLResponse, include_in_schema=False)
async def dashboard() -> HTMLResponse:
    return HTMLResponse(dashboard_html(""))


@app.get("/relay/health")
async def health() -> JSONResponse:
    return JSONResponse({"status": "ok", "service": SERVICE_NAME, "time": json_time(utc_now())})


@app.get("/relay/icon.svg", include_in_schema=False)
async def icon() -> Response:
    return Response(icon_svg(), media_type="image/svg+xml")


@app.get("/relay/status")
async def status(room: str | None = None) -> JSONResponse:
    return JSONResponse(await hub.snapshot(room))


@app.get("/relay/{room}", response_class=HTMLResponse, include_in_schema=False)
async def room_dashboard(room: str) -> HTMLResponse:
    return HTMLResponse(dashboard_html(room))


@app.websocket("/relay/ws")
async def relay_websocket_default(websocket: WebSocket) -> None:
    await relay_websocket(websocket, None)


@app.websocket("/relay/{room}/ws")
async def relay_websocket_room(websocket: WebSocket, room: str) -> None:
    await relay_websocket(websocket, room)


async def relay_websocket(websocket: WebSocket, room: str | None) -> None:
    role = read_role(websocket)
    token = room or (DASHBOARD_ROLE if role == DASHBOARD_ROLE else read_token(websocket))
    if role is None or token is None:
        await websocket.accept()
        await close_quietly(websocket, 1008, "missing relay role or bearer token")
        return

    await websocket.accept()
    await asyncio.sleep(0)
    remote = websocket_remote(websocket)
    logger.info("websocket connected role=%s room=%s remote=%s user_agent=%r", role, room_id(token), remote, websocket.headers.get("user-agent", ""))
    if role == DASHBOARD_ROLE:
        await hub.serve_dashboard(websocket, remote, room)
    elif role == "agent":
        await hub.serve_agent(token, websocket, remote, read_agent_identity(websocket), read_resumable(websocket), read_room_proof(websocket), read_service(websocket))
    elif role == "client":
        await hub.serve_client(token, websocket, remote, read_resumable(websocket), read_room_proof(websocket), read_service(websocket))
    elif role == RESUME_ROLE:
        await hub.serve_resume(token, websocket, remote, websocket.headers.get("x-deskferry-session"), websocket.headers.get("x-deskferry-session-side"), read_room_proof(websocket), read_service(websocket))
    elif role == "home-agent":
        await hub.serve_home_agent(token, websocket, remote, read_room_proof(websocket))
    elif role == "probe":
        await hub.serve_probe(token, websocket, read_room_proof(websocket))
    else:
        await close_quietly(websocket, 1008, "unsupported role")


def icon_svg() -> str:
    return """<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 108 108">
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
</svg>"""


def dashboard_html(room: str) -> str:
    room_json = json.dumps(room)
    return f"""<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>DeskFerry Relay</title>
  <link rel="icon" href="/relay/icon.svg" type="image/svg+xml">
  <style>
    :root {{
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
    }}
    * {{ box-sizing: border-box; }}
    body {{
      margin: 0;
      font-family: "Segoe UI", system-ui, -apple-system, BlinkMacSystemFont, sans-serif;
      background: var(--bg);
      color: var(--ink);
    }}
    header {{
      padding: 28px 24px 18px;
      border-bottom: 1px solid var(--line);
      background: var(--panel);
    }}
    main {{
      width: min(1120px, calc(100% - 32px));
      margin: 22px auto 40px;
    }}
    h1 {{
      margin: 0 0 6px;
      font-size: clamp(26px, 4vw, 38px);
      letter-spacing: 0;
    }}
    .brand {{
      display: flex;
      align-items: center;
      gap: 14px;
    }}
    .brand-icon {{
      width: 58px;
      height: 58px;
      flex: 0 0 58px;
      border-radius: 13px;
    }}
    .brand-text {{ min-width: 0; }}
    .subtle {{ color: var(--muted); }}
    .toolbar {{
      display: flex;
      gap: 10px;
      align-items: center;
      flex-wrap: wrap;
      margin-top: 16px;
    }}
    .toolbar input {{
      flex: 1 1 360px;
      min-width: 0;
      height: 40px;
      border: 1px solid var(--line);
      border-radius: 8px;
      padding: 0 12px;
      color: var(--ink);
      background: #fbfcfd;
      font: 13px ui-monospace, SFMono-Regular, Consolas, monospace;
    }}
    .toolbar button {{
      height: 40px;
      border: 1px solid var(--accent);
      border-radius: 8px;
      padding: 0 14px;
      color: var(--accent);
      background: #fff;
      font-weight: 700;
      cursor: pointer;
    }}
    .grid {{
      display: grid;
      grid-template-columns: repeat(3, minmax(0, 1fr));
      gap: 14px;
      margin-bottom: 18px;
    }}
    .card {{
      background: var(--panel);
      border: 1px solid var(--line);
      border-radius: 8px;
      padding: 16px;
      min-height: 128px;
    }}
    .label {{
      color: var(--muted);
      font-size: 13px;
      font-weight: 700;
      text-transform: uppercase;
    }}
    .value {{
      margin-top: 10px;
      font-size: 28px;
      font-weight: 700;
      line-height: 1.1;
    }}
    .ok {{ color: var(--ok); }}
    .warn {{ color: var(--warn); }}
    .bad {{ color: var(--bad); }}
    table {{
      width: 100%;
      border-collapse: collapse;
      background: var(--panel);
      border: 1px solid var(--line);
      border-radius: 8px;
      overflow: hidden;
    }}
    th, td {{
      padding: 12px 14px;
      text-align: left;
      border-bottom: 1px solid var(--line);
      vertical-align: top;
      font-size: 14px;
    }}
    th {{
      color: var(--muted);
      font-size: 12px;
      text-transform: uppercase;
      background: #fbfcfd;
    }}
    tr:last-child td {{ border-bottom: 0; }}
    code {{
      font-family: ui-monospace, SFMono-Regular, Consolas, monospace;
      font-size: 13px;
    }}
    .pill {{
      display: inline-block;
      padding: 3px 8px;
      border-radius: 999px;
      border: 1px solid var(--line);
      font-size: 12px;
      font-weight: 700;
      background: #f9fafb;
    }}
    .pill.ok {{ border-color: #bfe4cf; background: #edf8f1; }}
    .pill.bad {{ border-color: #efc5c5; background: #fff0f0; }}
    @media (max-width: 760px) {{
      .grid {{ grid-template-columns: 1fr; }}
      th:nth-child(5), td:nth-child(5) {{ display: none; }}
      .brand-icon {{
        width: 48px;
        height: 48px;
        flex-basis: 48px;
      }}
    }}
  </style>
</head>
<body>
  <header>
    <div class="brand">
      <img class="brand-icon" src="/relay/icon.svg" alt="">
      <div class="brand-text">
        <h1>DeskFerry Relay</h1>
        <div class="subtle">Python WebSocket relay at <code>/relay/ws</code>. Status updates stream live over WebSocket.</div>
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
    const pageRoom = {room_json};

    function pill(ok, text) {{
      return `<span class="pill ${{ok ? "ok" : "bad"}}">${{text}}</span>`;
    }}

    function esc(value) {{
      return String(value ?? "").replace(/[&<>"']/g, char => ({{
        "&": "&amp;",
        "<": "&lt;",
        ">": "&gt;",
        '"': "&quot;",
        "'": "&#39;"
      }}[char]));
    }}

    function fmt(value) {{
      if (!value) return "";
      return new Date(value).toLocaleString();
    }}

    function setValue(node, text, cls) {{
      node.className = "value " + cls;
      node.textContent = text;
    }}

    function relayRoomUrl(room) {{
      if (!room) return `${{location.origin}}/relay/`;
      return `${{location.origin}}/relay/${{encodeURIComponent(room)}}`;
    }}

    function render(data) {{
      const rooms = data.rooms || [];
      const waitingAgents = rooms.reduce((sum, r) => sum + (r.waiting_agents || 0), 0);
      const activePairs = rooms.reduce((sum, r) => sum + (r.active_pairs || 0), 0);
      const homeAgents = rooms.filter(r => r.home_agent_connected).length;
      const homeActiveRooms = rooms.filter(r => r.home_agent_connected || (r.active_pairs || 0) > 0).length;
      setValue(workStatus, waitingAgents + activePairs > 0 ? "Connected" : "Waiting", waitingAgents + activePairs > 0 ? "ok" : "warn");
      workDetail.textContent = `${{waitingAgents}} idle work sockets, ${{activePairs}} paired streams.`;
      setValue(homeStatus, homeActiveRooms > 0 ? "Active" : "Waiting", homeActiveRooms > 0 ? "ok" : "warn");
      homeDetail.textContent = `${{homeAgents}} presence socket${{homeAgents === 1 ? "" : "s"}}, ${{activePairs}} active RDP stream${{activePairs === 1 ? "" : "s"}}.`;
      streamStatus.textContent = activePairs.toString();
      streamDetail.textContent = activePairs === 0 ? "No active RDP streams." : `${{activePairs}} RDP stream${{activePairs === 1 ? "" : "s"}} bridged.`;
      if (rooms.length === 0) {{
        roomsBody.innerHTML = '<tr><td colspan="5" class="subtle">No rooms have connected yet.</td></tr>';
        return;
      }}
      roomsBody.innerHTML = rooms.map(r => {{
        const workConnected = (r.waiting_agents || 0) + (r.active_pairs || 0) > 0;
        const homePresence = !!r.home_agent_connected;
        const streamActive = (r.active_pairs || 0) > 0;
        const homeState = homePresence ? "presence" : (streamActive ? "active stream" : "waiting");
        const homeInfo = homePresence
          ? `${{esc(r.home_agent_remote || "")}}<br>${{esc(fmt(r.home_agent_connected_at))}}`
          : `${{r.active_pairs || 0}} active<br>${{esc(fmt(r.last_client_connected_at))}}`;
        return `<tr>
          <td><code>${{esc(r.id)}}</code></td>
          <td>${{pill(workConnected, workConnected ? "connected" : "waiting")}}<br><span class="subtle">${{r.waiting_agents || 0}} idle<br>${{esc(fmt(r.last_agent_connected_at))}}</span></td>
          <td>${{pill(homePresence || streamActive, homeState)}}<br><span class="subtle">${{homeInfo}}</span></td>
          <td>${{r.active_pairs || 0}}<br><span class="subtle">${{r.total_pairs || 0}} total</span></td>
          <td><span class="subtle">${{esc(r.last_client_remote || "")}}<br>${{esc(fmt(r.last_client_connected_at))}}</span></td>
        </tr>`;
      }}).join("");
    }}

    function connectDashboard() {{
      const scheme = location.protocol === "https:" ? "wss:" : "ws:";
      const roomPath = pageRoom ? `/relay/${{encodeURIComponent(pageRoom)}}/ws` : "/relay/ws";
      const socket = new WebSocket(`${{scheme}}//${{location.host}}${{roomPath}}?role=dashboard`);
      socket.onmessage = event => render(JSON.parse(event.data));
      socket.onclose = () => {{
        setValue(workStatus, "Reconnecting", "warn");
        setValue(homeStatus, "Reconnecting", "warn");
        setTimeout(connectDashboard, 1500);
      }};
      socket.onerror = () => socket.close();
    }}

    roomUrl.value = relayRoomUrl(pageRoom);
    copyRoom.addEventListener("click", async () => {{
      roomUrl.select();
      await navigator.clipboard.writeText(roomUrl.value);
    }});
    connectDashboard();
  </script>
</body>
</html>"""
