from __future__ import annotations

import asyncio
import struct
import hmac
import json
import logging
import time
import uuid
from collections import deque
from contextlib import asynccontextmanager, suppress
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

from fastapi import FastAPI, Request, WebSocket
from fastapi.responses import HTMLResponse, JSONResponse, RedirectResponse, Response, StreamingResponse
from starlette.requests import ClientDisconnect
from starlette.websockets import WebSocketDisconnect, WebSocketState

SERVICE_NAME = "DeskFerry.Relay"
RELAY_VERSION = "0.11.4"
DASHBOARD_ROLE = "dashboard"
RESUME_ROLE = "resume"
STARTED = "started"
AGENT_UNAVAILABLE = "agent-unavailable"
CLIENT_UNAVAILABLE = "client-unavailable"
VALID_ROLES = {"agent", "agent-control", "agent-session", "client", "home-agent", "probe", "diagnostic-log", RESUME_ROLE, DASHBOARD_ROLE}
PROTOCOL_VERSION = 2
SESSION_OFFER_SECONDS = 8

logger = logging.getLogger("deskferry.relay")
logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
logger.info("DeskFerry Python relay version=%s", RELAY_VERSION)

@asynccontextmanager
async def app_lifespan(_: FastAPI):
    sweeper = asyncio.create_task(sweep_http_streams())
    try:
        yield
    finally:
        sweeper.cancel()
        with suppress(asyncio.CancelledError):
            await sweeper


app = FastAPI(title="DeskFerry Python Relay", docs_url=None, redoc_url=None, lifespan=app_lifespan)

HTTP_STREAM_ACK = 0
HTTP_STREAM_TEXT = 1
HTTP_STREAM_BINARY = 2
HTTP_STREAM_CLOSE = 8
HTTP_STREAM_HEADER = 13
HTTP_STREAM_LIMIT = 1 << 20
HTTP_STREAM_BUFFER = 8 * 1024 * 1024
HTTP_STREAM_KEEPALIVE = 10
HTTP_STREAM_RETENTION = 300


@dataclass
class HTTPStreamFrame:
    kind: int
    sequence: int
    payload: bytes = b""


class HTTPStreamWebSocket:
    """Starlette WebSocket-compatible facade over a reliable POST/GET pair."""

    def __init__(self, request: Request, secret: str) -> None:
        self.headers = request.headers
        self.query_params = request.query_params
        self.client = request.client
        self.client_state = WebSocketState.CONNECTED
        self.application_state = WebSocketState.CONNECTED
        self.secret = secret
        self.incoming: asyncio.Queue[HTTPStreamFrame] = asyncio.Queue()
        self.outgoing: list[HTTPStreamFrame] = []
        self.outgoing_bytes = 0
        self.next_send = 1
        self.next_receive = 1
        self.changed = asyncio.Event()
        self.lock = asyncio.Lock()
        self.up_generation = 0
        self.down_generation = 0
        self.down_batch = False
        self.down_primed = False
        self.last_activity = time.monotonic()
        self.closed = asyncio.Event()

    async def receive(self) -> dict[str, Any]:
        frame = await self.incoming.get()
        if frame.kind == HTTP_STREAM_CLOSE:
            code = struct.unpack(">H", frame.payload[:2])[0] if len(frame.payload) >= 2 else 1000
            reason = frame.payload[2:].decode("utf-8", errors="replace") if len(frame.payload) > 2 else ""
            self.client_state = WebSocketState.DISCONNECTED
            return {"type": "websocket.disconnect", "code": code, "reason": reason}
        if frame.kind == HTTP_STREAM_TEXT:
            return {"type": "websocket.receive", "text": frame.payload.decode("utf-8")}
        return {"type": "websocket.receive", "bytes": frame.payload}

    async def receive_text(self) -> str:
        message = await self.receive()
        if message["type"] == "websocket.disconnect":
            raise WebSocketDisconnect(message.get("code", 1000), message.get("reason", ""))
        if "text" not in message:
            raise WebSocketDisconnect(1003, "text message required")
        return message["text"]

    async def receive_json(self) -> Any:
        return json.loads(await self.receive_text())

    async def send_text(self, value: str) -> None:
        await self._send(HTTP_STREAM_TEXT, value.encode("utf-8"))

    async def send_bytes(self, value: bytes) -> None:
        await self._send(HTTP_STREAM_BINARY, bytes(value))

    async def send_json(self, value: Any) -> None:
        await self.send_text(json.dumps(value, separators=(",", ":")))

    async def close(self, code: int = 1000, reason: str = "") -> None:
        if self.application_state != WebSocketState.CONNECTED:
            return
        reason_bytes = reason.encode("utf-8")[:123]
        await self._send(HTTP_STREAM_CLOSE, struct.pack(">H", code) + reason_bytes)
        self.application_state = WebSocketState.DISCONNECTED
        self.closed.set()

    async def _send(self, kind: int, payload: bytes) -> None:
        if len(payload) > HTTP_STREAM_LIMIT:
            raise WebSocketDisconnect(1009, "message exceeds relay limit")
        while True:
            async with self.lock:
                if self.application_state != WebSocketState.CONNECTED:
                    raise WebSocketDisconnect(1006, "HTTP stream is closed")
                if self.outgoing_bytes + len(payload) <= HTTP_STREAM_BUFFER:
                    self.outgoing.append(HTTPStreamFrame(kind, self.next_send, payload))
                    self.next_send += 1
                    self.outgoing_bytes += len(payload)
                    self.changed.set()
                    return
            await asyncio.sleep(0.01)

    async def apply(self, frame: HTTPStreamFrame) -> None:
        async with self.lock:
            self.last_activity = time.monotonic()
            if frame.kind == HTTP_STREAM_ACK:
                if frame.sequence >= self.next_send:
                    raise ValueError("HTTP stream acknowledgement exceeds sent sequence")
                while self.outgoing and self.outgoing[0].sequence <= frame.sequence:
                    self.outgoing_bytes -= len(self.outgoing.pop(0).payload)
                return
            if frame.sequence < self.next_receive:
                self.changed.set()
                return
            if frame.sequence != self.next_receive or frame.kind not in {HTTP_STREAM_TEXT, HTTP_STREAM_BINARY, HTTP_STREAM_CLOSE}:
                raise ValueError("invalid HTTP stream sequence or record type")
            self.next_receive += 1
            self.incoming.put_nowait(frame)
            self.changed.set()

    async def snapshot(self, last_sequence: int) -> tuple[list[HTTPStreamFrame], int]:
        async with self.lock:
            # Clear while holding the same lock used by producers so a frame
            # queued after this snapshot cannot lose its wake-up signal.
            self.changed.clear()
            return ([frame for frame in self.outgoing if frame.sequence > last_sequence], self.next_receive - 1)

    async def serve_upload(self, request: Request) -> Response:
        if request.method != "POST":
            return Response("HTTP stream upstream requires POST", status_code=405)
        self.up_generation += 1
        generation = self.up_generation
        buffered = bytearray()
        try:
            async for chunk in request.stream():
                if generation != self.up_generation:
                    break
                buffered.extend(chunk)
                while len(buffered) >= HTTP_STREAM_HEADER:
                    kind, sequence, length = struct.unpack(">BQI", buffered[:HTTP_STREAM_HEADER])
                    if length > HTTP_STREAM_LIMIT:
                        raise ValueError("HTTP stream record exceeds relay limit")
                    total = HTTP_STREAM_HEADER + length
                    if len(buffered) < total:
                        break
                    payload = bytes(buffered[HTTP_STREAM_HEADER:total])
                    del buffered[:total]
                    await self.apply(HTTPStreamFrame(kind, sequence, payload))
        except (asyncio.CancelledError, ClientDisconnect, ConnectionError):
            pass
        return Response(status_code=204)

    async def serve_download(self, request: Request) -> Response:
        self.down_generation += 1
        generation = self.down_generation

        if self.down_batch:
            prime = not self.down_primed
            self.down_primed = True
            while generation == self.down_generation and not await request.is_disconnected():
                frames, ack = await self.snapshot(0)
                if prime or frames:
                    payload = encode_http_stream_record(HTTPStreamFrame(HTTP_STREAM_ACK, ack))
                    payload += b"".join(encode_http_stream_record(frame) for frame in frames)
                    return Response(payload, media_type="application/octet-stream", headers={
                        "Cache-Control": "no-store, no-transform",
                    })
                try:
                    await asyncio.wait_for(self.changed.wait(), timeout=HTTP_STREAM_KEEPALIVE)
                except asyncio.TimeoutError:
                    payload = encode_http_stream_record(HTTPStreamFrame(HTTP_STREAM_ACK, ack))
                    return Response(payload, media_type="application/octet-stream", headers={
                        "Cache-Control": "no-store, no-transform",
                    })
            return Response(status_code=204)

        async def records():
            last_sequence = 0
            last_ack = 0
            force_ack = True
            while generation == self.down_generation and not await request.is_disconnected():
                frames, ack = await self.snapshot(last_sequence)
                if force_ack or ack > last_ack:
                    yield encode_http_stream_record(HTTPStreamFrame(HTTP_STREAM_ACK, ack))
                    last_ack = ack
                    force_ack = False
                for frame in frames:
                    yield encode_http_stream_record(frame)
                    last_sequence = frame.sequence
                try:
                    await asyncio.wait_for(self.changed.wait(), timeout=HTTP_STREAM_KEEPALIVE)
                except asyncio.TimeoutError:
                    force_ack = True

        return StreamingResponse(records(), media_type="application/octet-stream", headers={
            "Cache-Control": "no-store, no-transform",
            "X-Accel-Buffering": "no",
        })


def encode_http_stream_record(frame: HTTPStreamFrame) -> bytes:
    return struct.pack(">BQI", frame.kind, frame.sequence, len(frame.payload)) + frame.payload


http_streams: dict[str, HTTPStreamWebSocket] = {}
http_streams_lock = asyncio.Lock()


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


def clean_log_label(value: str, limit: int) -> str:
    cleaned = (value or "").replace("\r", "").replace("\n", "").strip()
    return (cleaned[:limit] if cleaned else "unknown")


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


def read_heartbeat(websocket: WebSocket) -> bool:
    return websocket.headers.get("x-deskferry-heartbeat", "").strip().lower() in {"1", "true"}


def read_agent_services(websocket: WebSocket) -> set[str]:
    return {
        value.strip().lower()
        for value in websocket.headers.get("x-deskferry-agent-services", "").split(",")
        if value.strip().lower() in {"rdp", "winrm", "smb", "screen"}
    }


def read_concurrency(websocket: WebSocket) -> int:
    try:
        value = int(websocket.headers.get("x-deskferry-concurrency", "4"))
    except ValueError:
        return 4
    return value if 1 <= value <= 256 else 4


def read_room_proof(websocket: WebSocket) -> str:
    return websocket.headers.get("x-deskferry-room-proof", "").strip()


def read_service(websocket: WebSocket) -> str:
    service = websocket.headers.get("x-deskferry-service", "").strip().lower()
    return service if service in {"winrm", "smb", "screen"} else "rdp"


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


async def send_v2(websocket: WebSocket, message: dict[str, Any]) -> bool:
    message = {**message, "protocol_version": message.get("protocol_version", PROTOCOL_VERSION)}
    try:
        await asyncio.wait_for(websocket.send_json(message), timeout=5)
        return True
    except (asyncio.CancelledError, WebSocketDisconnect):
        return False
    except Exception:
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
        self._control_connections = 0
        self._pending_requests = 0
        self._busy_rejections = 0
        self._no_agent_rejections = 0
        self._control_agents: dict[str, int] = {}
        self._pending_by_service: dict[str, int] = {}
        self._active_by_service: dict[str, int] = {}
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
            if not self._credential_set or (not self._agents and self._control_connections == 0 and self._active_pairs == 0):
                self._credential_set = True
                self._room_proof = proof
                return True
            return hmac.compare_digest(self._room_proof, proof)

    async def authorize_client(self, proof: str) -> bool:
        async with self._lock:
            return (not self._credential_set and not proof) or (
                self._credential_set and hmac.compare_digest(self._room_proof, proof)
            )

    async def credential_set(self) -> bool:
        async with self._lock:
            return self._credential_set

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

    async def has_waiting_agent(self, service: str) -> bool:
        async with self._lock:
            self._prune_closed_agents_locked()
            return any(waiting.is_open and waiting.service == service for waiting in self._agents)

    async def control_connected(self, agent_id: str, remote: str) -> None:
        async with self._lock:
            self._control_connections += 1
            self._control_agents[agent_id] = self._control_agents.get(agent_id, 0) + 1
            self._last_agent_remote = remote
            self._last_agent_connected_at = utc_now()

    async def control_disconnected(self, agent_id: str) -> None:
        async with self._lock:
            self._control_connections = max(0, self._control_connections - 1)
            count = self._control_agents.get(agent_id, 0)
            if count <= 1:
                self._control_agents.pop(agent_id, None)
            else:
                self._control_agents[agent_id] = count - 1
            self._last_agent_disconnected_at = utc_now()

    async def pending_started(self, service: str) -> None:
        async with self._lock:
            self._pending_requests += 1
            self._pending_by_service[service] = self._pending_by_service.get(service, 0) + 1

    async def pending_ended(self, service: str) -> None:
        async with self._lock:
            self._pending_requests = max(0, self._pending_requests - 1)
            self._pending_by_service[service] = max(0, self._pending_by_service.get(service, 0) - 1)

    async def service_session_started(self, service: str) -> None:
        async with self._lock:
            self._active_by_service[service] = self._active_by_service.get(service, 0) + 1

    async def service_session_ended(self, service: str) -> None:
        async with self._lock:
            self._active_by_service[service] = max(0, self._active_by_service.get(service, 0) - 1)

    async def record_rejection(self, result: str) -> None:
        async with self._lock:
            if result == "busy":
                self._busy_rejections += 1
            elif result == "no-agent":
                self._no_agent_rejections += 1

    async def remove_waiting(self, waiting: WaitingAgent) -> None:
        async with self._lock:
            self._agents = deque(agent for agent in self._agents if agent is not waiting)
            self._last_agent_disconnected_at = utc_now()

    async def remove_legacy_agents(self, instance: str) -> int:
        removed: list[WaitingAgent] = []
        async with self._lock:
            self._prune_closed_agents_locked()
            kept: deque[WaitingAgent] = deque()
            while self._agents:
                agent = self._agents.popleft()
                if instance and agent.identity.instance == instance:
                    agent.try_cancel()
                    removed.append(agent)
                else:
                    kept.append(agent)
            self._agents = kept
        for agent in removed:
            await close_quietly(agent.websocket, 1000, "replaced by protocol v2 control connection")
        return len(removed)

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
                "control_connections": self._control_connections,
                "pending_requests": self._pending_requests,
                "busy_rejections": self._busy_rejections,
                "no_agent_rejections": self._no_agent_rejections,
                "control_agents": sorted(self._control_agents),
                "protocol_version": PROTOCOL_VERSION,
                "pending_by_service": dict(self._pending_by_service),
                "active_sessions_by_service": dict(self._active_by_service),
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
        await self._run(agent, client, client_done, state_changed, None, None)

    async def run_recovered(self, state_changed: Any) -> None:
        try:
            while not self._done.is_set():
                try:
                    agent_attachment, client_attachment = await asyncio.wait_for(
                        asyncio.gather(self._agent.get(), self._client.get()), timeout=300
                    )
                except (asyncio.TimeoutError, asyncio.CancelledError):
                    return
                if not await send_control(agent_attachment.websocket, f"resume {self.id}", "agent", self.room.id, agent_attachment.remote) or not await send_control(client_attachment.websocket, f"resume {self.id}", "client", self.room.id, client_attachment.remote):
                    await close_quietly(agent_attachment.websocket, 1012, "retry resume")
                    await close_quietly(client_attachment.websocket, 1012, "retry resume")
                    try_set_result(agent_attachment.done, None)
                    try_set_result(client_attachment.done, None)
                    continue
                self.agent_remote = agent_attachment.remote
                self.client_remote = client_attachment.remote
                await self.room.service_session_started(self.service)
                try:
                    logger.info("reconstructed resumable bridge ready room=%s session=%s service=%s agent=%s client=%s", self.room.id, self.id, self.service, self.agent_remote, self.client_remote)
                    await self._run(
                        agent_attachment.websocket, client_attachment.websocket, None,
                        state_changed, agent_attachment, client_attachment,
                    )
                finally:
                    await self.room.service_session_ended(self.service)
                return
        finally:
            self.finish()

    async def _run(
        self,
        agent: WebSocket,
        client: WebSocket,
        client_done: asyncio.Future[None] | None,
        state_changed: Any,
        agent_attachment: ResumeAttachment | None,
        client_attachment: ResumeAttachment | None,
    ) -> None:
        started_at = time.monotonic()
        pair_id = await self.room.pair_started(self.client_remote)
        state_changed()
        try:
            while True:
                first, second = await bridge_once(agent, client)
                # bridge_once cancels the opposite pump after the first one
                # ends. A close observed by that canceled pump did not
                # initiate shutdown and must not complete an otherwise
                # resumable session.
                if is_session_close(first):
                    await close_quietly(agent, 1000, "session closed")
                    await close_quietly(client, 1000, "session closed")
                    if agent_attachment is not None:
                        try_set_result(agent_attachment.done, None)
                    if client_attachment is not None:
                        try_set_result(client_attachment.done, None)
                    return

                logger.info(
                    "resumable bridge interrupted room=%s pair=%s session=%s trigger_direction=%s trigger_end=%s trigger_close_code=%s trigger_close_reason=%r trigger_error=%r other_direction=%s other_end=%s other_close_code=%s other_close_reason=%r other_error=%r",
                    self.room.id, pair_id, self.id, first.direction, first.end, first.close_code, first.close_reason, first.error, second.direction, second.end, second.close_code, second.close_reason, second.error,
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
            if client_done is not None:
                try_set_result(client_done, None)
            self.finish()
            state_changed()
            logger.info("resumable bridge closed room=%s pair=%s session=%s agent=%s client=%s duration_ms=%d", self.room.id, pair_id, self.id, self.agent_remote, self.client_remote, round((time.monotonic() - started_at) * 1000))

    def finish(self) -> None:
        if self._done.is_set():
            return
        self._done.set()
        self._on_finish(self)


def is_session_close(result: PumpResult) -> bool:
    return result.end == "close-frame" and result.close_code == 1000 and result.close_reason == "session closed"


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


@dataclass
class AgentControl:
    room: RelayRoom
    websocket: WebSocket
    remote: str
    agent_id: str
    services: set[str]
    concurrency: int
    send_lock: asyncio.Lock = field(default_factory=asyncio.Lock)
    in_use: int = 0
    closed: bool = False

    def reserve(self) -> bool:
        if self.closed or self.in_use >= self.concurrency:
            return False
        self.in_use += 1
        return True

    def release(self) -> None:
        self.in_use = max(0, self.in_use - 1)

    async def send(self, message: dict[str, Any]) -> bool:
        async with self.send_lock:
            return not self.closed and await send_v2(self.websocket, message)


@dataclass
class AgentDataSocket:
    websocket: WebSocket
    remote: str
    resumable: bool
    done: asyncio.Future[None] = field(default_factory=asyncio.Future)


@dataclass
class PendingSession:
    id: str
    room: RelayRoom
    control: AgentControl
    client: WebSocket
    remote: str
    proof: str
    service: str
    resumable: bool
    heartbeat: bool
    created_at: datetime
    expires_at: datetime
    response: asyncio.Future[dict[str, Any]] = field(default_factory=asyncio.Future)
    agent: asyncio.Future[AgentDataSocket] = field(default_factory=asyncio.Future)


class RelayHub:
    def __init__(self) -> None:
        self._lock = asyncio.Lock()
        self._rooms: dict[str, RelayRoom] = {}
        self._dashboards: dict[str, DashboardClient] = {}
        self._sessions: dict[str, ResumeSession] = {}
        self._completed_sessions: dict[str, float] = {}
        self._controls: dict[str, AgentControl] = {}
        self._pending: dict[str, PendingSession] = {}

    async def serve_agent_control(
        self, token: str, websocket: WebSocket, remote: str, agent_id: str,
        services: set[str], concurrency: int, proof: str = "",
    ) -> None:
        room = await self._room_for(token)
        if not agent_id or not services:
            await send_v2(websocket, {"type": "invalid-request", "reason": "agent identity and services are required"})
            await close_quietly(websocket, 1008, "invalid agent control request")
            return
        if not await room.authorize_agent(proof):
            await send_v2(websocket, {"type": "authentication-failed", "reason": "room authentication failed"})
            await close_quietly(websocket, 1008, "room authentication failed")
            return
        control = AgentControl(room, websocket, remote, agent_id, services, concurrency)
        key = f"{room.id}/{agent_id}"
        previous = self._controls.get(key)
        self._controls[key] = control
        await room.control_connected(agent_id, remote)
        removed_legacy = await room.remove_legacy_agents(agent_id)
        if previous is not None:
            previous.closed = True
            await close_quietly(previous.websocket, 1000, "replaced by newer control connection")
        self.notify_dashboards()
        try:
            if not await control.send({"type": "control-ready", "agent_id": agent_id}):
                return
            logger.info("agent control connected room=%s agent=%s services=%s concurrency=%s remote=%s removed_legacy_slots=%s", room.id, agent_id, sorted(services), concurrency, remote, removed_legacy)
            while websocket_is_connected(websocket):
                message = await websocket.receive_json()
                if not isinstance(message, dict):
                    continue
                message_type = str(message.get("type", "")).strip().lower()
                session_id = clean_session_value(str(message.get("session_id", "")))
                if not session_id:
                    continue
                if message_type == "session-closed":
                    session = self._sessions.get(f"{room.id}/{session_id}")
                    if session is not None:
                        session.finish()
                    continue
                pending = self._pending.get(f"{room.id}/{session_id}")
                if pending is not None and pending.control is control and message_type in {"accept", "busy", "service-disabled", "unsupported-version"}:
                    message["type"] = message_type
                    try_set_result(pending.response, message)
        except (asyncio.CancelledError, WebSocketDisconnect):
            pass
        except Exception:
            logger.exception("agent control ended room=%s agent=%s remote=%s", room.id, agent_id, remote)
        finally:
            if self._controls.get(key) is control:
                self._controls.pop(key, None)
            control.closed = True
            for pending in list(self._pending.values()):
                if pending.control is control:
                    try_set_result(pending.response, {"type": "no-agent", "session_id": pending.id, "reason": "work control disconnected"})
            await close_quietly(websocket)
            await room.control_disconnected(agent_id)
            self.notify_dashboards()

    async def serve_v2_client(
        self, token: str, websocket: WebSocket, remote: str, resumable: bool = False,
        proof: str = "", service: str = "rdp", heartbeat: bool = False,
    ) -> None:
        room = await self._room_for(token)
        if not await room.authorize_client(proof):
            await send_v2(websocket, {"type": "authentication-failed", "reason": "room authentication failed"})
            await close_quietly(websocket, 1008, "room authentication failed")
            return
        control = self._select_control(room.id, service)
        if control is None:
            if await room.has_waiting_agent(service):
                await self.serve_client(token, websocket, remote, resumable, proof, service)
                return
            service_control_exists = any(
                key.startswith(f"{room.id}/") and service in value.services
                for key, value in self._controls.items()
            )
            result = "busy" if service_control_exists else "no-agent"
            reason = "work agent concurrency limit reached" if service_control_exists else "no work agent control connection"
            await room.record_rejection(result)
            await send_v2(websocket, {"type": result, "reason": reason})
            await close_quietly(websocket, 1000, reason)
            return
        await self._serve_on_demand_client(room, websocket, remote, resumable, heartbeat, proof, service, control, True)

    async def _serve_on_demand_client(
        self, room: RelayRoom, websocket: WebSocket, remote: str, resumable: bool,
        heartbeat: bool, proof: str, service: str, control: AgentControl, typed: bool,
    ) -> None:
        if len(self._pending) >= 4096:
            control.release()
            await room.record_rejection("busy")
            await self._reject_session_client(websocket, typed, "busy", "", "relay pending-session limit reached")
            return
        created_at = utc_now()
        expires_at = datetime.fromtimestamp(created_at.timestamp() + SESSION_OFFER_SECONDS, timezone.utc)
        pending = PendingSession(uuid.uuid4().hex, room, control, websocket, remote, proof, service, resumable, heartbeat, created_at, expires_at)
        key = f"{room.id}/{pending.id}"
        self._pending[key] = pending
        await room.pending_started(service)
        self.notify_dashboards()
        pending_open = True
        try:
            offer = {
                "type": "session-offer", "session_id": pending.id, "room": room.id,
                "service": service, "agent_id": control.agent_id,
                "created_at": json_time(created_at), "expires_at": json_time(expires_at),
                "resumable": resumable,
                "heartbeat": heartbeat,
            }
            if not await control.send(offer):
                await room.record_rejection("no-agent")
                await self._reject_session_client(websocket, typed, "no-agent", pending.id, "work control disconnected")
                return
            try:
                response = await asyncio.wait_for(asyncio.shield(pending.response), SESSION_OFFER_SECONDS)
            except asyncio.TimeoutError:
                await room.record_rejection("timeout")
                await self._reject_session_client(websocket, typed, "timeout", pending.id, "work agent did not answer the offer")
                return
            if response.get("type") != "accept":
                result = str(response.get("type") or "invalid-request")
                await room.record_rejection(result)
                await self._reject_session_client(websocket, typed, result, pending.id, str(response.get("reason") or ""))
                return
            remaining = max(0.001, pending.expires_at.timestamp() - utc_now().timestamp())
            try:
                agent = await asyncio.wait_for(asyncio.shield(pending.agent), remaining)
            except asyncio.TimeoutError:
                await room.record_rejection("timeout")
                await self._reject_session_client(websocket, typed, "timeout", pending.id, "accepted work session did not connect")
                return
            self._pending.pop(key, None)
            await room.pending_ended(service)
            pending_open = False
            heartbeat = pending.heartbeat and bool(response.get("heartbeat"))
            ready = {"type": "session-ready", "session_id": pending.id, "service": service, "heartbeat": heartbeat}
            client_ready = await send_v2(websocket, ready) if typed else await send_control(websocket, f"start {pending.id}", "legacy-client", room.id, remote)
            if not await send_v2(agent.websocket, ready) or not client_ready:
                await close_quietly(agent.websocket)
                return
            logger.info("v2 pairing room=%s session=%s service=%s agent=%s client=%s", room.id, pending.id, service, agent.remote, remote)
            await room.service_session_started(service)
            try:
                if resumable and agent.resumable:
                    session = self._new_resume_session(room, agent.remote, remote, proof, service, pending.id)
                    await session.run(agent.websocket, websocket, agent.done, self.notify_dashboards)
                else:
                    await room.bridge(agent.websocket, websocket, agent.remote, remote, agent.done, self.notify_dashboards)
            finally:
                await room.service_session_ended(service)
        except (asyncio.CancelledError, WebSocketDisconnect):
            pass
        finally:
            if pending_open:
                self._pending.pop(key, None)
                await room.pending_ended(service)
            control.release()
            self.notify_dashboards()

    async def _reject_session_client(self, websocket: WebSocket, typed: bool, result: str, session_id: str, reason: str) -> None:
        if typed:
            await send_v2(websocket, {"type": result, "session_id": session_id, "reason": reason})
        await close_quietly(websocket, 1013, reason)

    async def serve_agent_session(
        self, token: str, websocket: WebSocket, remote: str, agent_id: str,
        session_id: str | None, resumable: bool, proof: str, service: str,
    ) -> None:
        session_id = clean_session_value(session_id)
        pending = self._pending.get(f"{room_id(token)}/{session_id}")
        if (not session_id or pending is None or pending.control.agent_id != agent_id or pending.service != service or
                not hmac.compare_digest(pending.proof, proof) or utc_now() >= pending.expires_at):
            await send_v2(websocket, {"type": "invalid-request", "session_id": session_id, "reason": "unknown or expired pending session"})
            await close_quietly(websocket, 1008, "unknown pending session")
            return
        data = AgentDataSocket(websocket, remote, resumable)
        if pending.agent.done():
            await close_quietly(websocket, 1008, "duplicate agent session")
            return
        pending.agent.set_result(data)
        try:
            await data.done
        except asyncio.CancelledError:
            pass

    def _select_control(self, room: str, service: str) -> AgentControl | None:
        controls = sorted(
            (value for key, value in self._controls.items() if key.startswith(f"{room}/") and service in value.services),
            key=lambda value: value.in_use,
        )
        return next((control for control in controls if control.reserve()), None)

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
        control = self._select_control(room.id, service)
        if control is not None:
            await self._serve_on_demand_client(room, websocket, remote, resumable, False, proof, service, control, False)
            return
        if any(key.startswith(f"{room.id}/") and service in value.services for key, value in self._controls.items()):
            await room.record_rejection("busy")
            await close_quietly(websocket, 1013, "work agent concurrency limit reached")
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
        room_key = room_id(token)
        if not session_id or side not in {"agent", "client"}:
            await close_quietly(websocket, 1008, "unknown resumable session")
            return
        key = f"{room_key}/{session_id}"
        session = self._sessions.get(key)
        if session is None:
            completed_until = self._completed_sessions.get(key, 0.0)
            if completed_until > time.monotonic():
                logger.info("resume rejected for completed session room=%s session=%s side=%s remote=%s", room_key, session_id, side, remote)
                await close_quietly(websocket, 1008, "unknown resumable session")
                return
            self._completed_sessions.pop(key, None)
            if side == "agent":
                room = await self._room_for(token)
                authorized = await room.authorize_agent(proof)
            else:
                async with self._lock:
                    room = self._rooms.get(room_key)
                if room is None or not await room.credential_set():
                    logger.info("resume room not ready room=%s session=%s side=%s remote=%s", room_key, session_id, side, remote)
                    await close_quietly(websocket, 1013, "resume room not ready")
                    return
                authorized = await room.authorize_client(proof)
            if not authorized:
                logger.info("resume authentication failed room=%s session=%s side=%s remote=%s", room_key, session_id, side, remote)
                await close_quietly(websocket, 1008, "room authentication failed")
                return
            created = False
            async with self._lock:
                session = self._sessions.get(key)
                if session is None and len(self._sessions) < 4096:
                    agent_remote = remote if side == "agent" else ""
                    client_remote = remote if side == "client" else ""
                    session = self._new_resume_session(room, agent_remote, client_remote, proof, service, session_id)
                    created = True
            if session is None:
                await close_quietly(websocket, 1013, "relay resumable-session limit reached")
                return
            if created:
                logger.info("reconstructed resumable session room=%s session=%s service=%s first_side=%s remote=%s", room_key, session_id, service, side, remote)
                asyncio.create_task(self._run_recovered_session(session))
        if session.service != service or not hmac.compare_digest(session.room_proof, proof):
            await close_quietly(websocket, 1008, "unknown resumable session")
            return
        if not await session.attach(side, websocket, remote):
            await close_quietly(websocket, 1013, "resumable session unavailable")

    async def _run_recovered_session(self, session: ResumeSession) -> None:
        try:
            await session.run_recovered(self.notify_dashboards)
        except Exception:
            logger.exception("reconstructed resumable bridge failed room=%s session=%s service=%s", session.room.id, session.id, session.service)
            session.finish()

    def _new_resume_session(self, room: RelayRoom, agent_remote: str, client_remote: str, proof: str, service: str, session_id: str | None = None) -> ResumeSession:
        def remove(session: ResumeSession) -> None:
            key = f"{room.id}/{session.id}"
            self._sessions.pop(key, None)
            self._completed_sessions[key] = time.monotonic() + 5 * 60
            if len(self._completed_sessions) > 4096:
                now = time.monotonic()
                self._completed_sessions = {
                    completed_key: expires_at
                    for completed_key, expires_at in self._completed_sessions.items()
                    if expires_at > now
                }

        session = ResumeSession(session_id or uuid.uuid4().hex, room, agent_remote, client_remote, remove, proof, service)
        key = f"{room.id}/{session.id}"
        self._completed_sessions.pop(key, None)
        self._sessions[key] = session
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

    async def serve_diagnostic_log(self, token: str, websocket: WebSocket, remote: str, proof: str, component: str, instance: str) -> None:
        room = await self._room_for(token)
        if not await room.authorize_client(proof):
            await close_quietly(websocket, 1008, "room authentication failed")
            return
        component = clean_log_label(component, 64)
        instance = clean_log_label(instance, 128)
        while True:
            try:
                payload = await websocket.receive_text()
            except WebSocketDisconnect:
                return
            try:
                entries = json.loads(payload).get("entries")
            except (json.JSONDecodeError, AttributeError):
                entries = None
            if not isinstance(entries, list) or not 1 <= len(entries) <= 100 or not all(isinstance(entry, str) for entry in entries):
                await close_quietly(websocket, 1008, "invalid diagnostic log batch")
                return
            for entry in entries:
                entry = entry.replace("\r", " ").replace("\n", " ")[:8192]
                logger.info("agent_log room=%s component=%s instance=%s remote=%s message=%r", room.id, component, instance, remote, entry)
            await websocket.send_json({"accepted": len(entries)})

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
    return JSONResponse({"status": "ok", "service": SERVICE_NAME, "version": RELAY_VERSION, "time": json_time(utc_now())})


@app.get("/relay/icon.svg", include_in_schema=False)
async def icon() -> Response:
    return Response(icon_svg(), media_type="image/svg+xml")


@app.get("/relay/status")
async def status(room: str | None = None) -> JSONResponse:
    return JSONResponse(await hub.snapshot(room))


@app.api_route("/relay/stream/{stream_id}/{direction}", methods=["GET", "POST"])
@app.api_route("/relay/{room}/stream/{stream_id}/{direction}", methods=["GET", "POST"])
async def relay_http_stream(request: Request, stream_id: str, direction: str, room: str | None = None) -> Response:
    secret = request.headers.get("x-deskferry-stream-secret", "").strip()
    direction = direction.strip().lower()
    if not 16 <= len(stream_id) <= 128 or not 24 <= len(secret) <= 256 or direction not in {"up", "down"}:
        return Response("Invalid HTTP stream request.", status_code=400)
    key = f"{(room or '').lower()}/{stream_id}"
    created = False
    async with http_streams_lock:
        websocket = http_streams.get(key)
        if websocket is None:
            if len(http_streams) >= 4096:
                return Response("HTTP stream capacity reached.", status_code=503)
            websocket = HTTPStreamWebSocket(request, secret)
            http_streams[key] = websocket
            created = True
    if not hmac.compare_digest(websocket.secret, secret):
        return Response("HTTP stream secret mismatch.", status_code=403)
    batch_header = request.headers.get("x-deskferry-stream-batch", "").strip().lower()
    if batch_header in {"1", "true"}:
        websocket.down_batch = True
    if created:
        asyncio.create_task(serve_http_stream_socket(websocket, room))
    if direction == "up":
        return await websocket.serve_upload(request)
    return await websocket.serve_download(request)


async def serve_http_stream_socket(websocket: HTTPStreamWebSocket, room: str | None) -> None:
    try:
        await serve_relay_socket(websocket, room, "http-stream")
    except Exception:
        logger.exception("HTTP stream relay handler failed room=%s", room_id(room))
        await close_quietly(websocket, 1011, "relay handler failed")


async def sweep_http_streams() -> None:
    while True:
        await asyncio.sleep(60)
        cutoff = time.monotonic() - HTTP_STREAM_RETENTION
        async with http_streams_lock:
            expired = [key for key, value in http_streams.items() if value.last_activity < cutoff]
            for key in expired:
                stream = http_streams.pop(key)
                stream.application_state = WebSocketState.DISCONNECTED
                stream.client_state = WebSocketState.DISCONNECTED
                stream.incoming.put_nowait(HTTPStreamFrame(HTTP_STREAM_CLOSE, stream.next_receive, struct.pack(">H", 1001) + b"HTTP stream expired"))
                stream.closed.set()


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
    await serve_relay_socket(websocket, room, "websocket")


async def serve_relay_socket(websocket: WebSocket, room: str | None, transport: str) -> None:
    role = read_role(websocket)
    token = room or (DASHBOARD_ROLE if role == DASHBOARD_ROLE else read_token(websocket))
    if role is None or token is None:
        await close_quietly(websocket, 1008, "missing relay role or bearer token")
        return
    remote = websocket_remote(websocket)
    logger.info("relay transport connected transport=%s role=%s room=%s remote=%s user_agent=%r", transport, role, room_id(token), remote, websocket.headers.get("user-agent", ""))
    if role == DASHBOARD_ROLE:
        await hub.serve_dashboard(websocket, remote, room)
    elif role == "agent":
        await hub.serve_agent(token, websocket, remote, read_agent_identity(websocket), read_resumable(websocket), read_room_proof(websocket), read_service(websocket))
    elif role == "agent-control":
        await hub.serve_agent_control(token, websocket, remote, read_agent_identity(websocket).instance, read_agent_services(websocket), read_concurrency(websocket), read_room_proof(websocket))
    elif role == "agent-session":
        await hub.serve_agent_session(token, websocket, remote, read_agent_identity(websocket).instance, websocket.headers.get("x-deskferry-session"), read_resumable(websocket), read_room_proof(websocket), read_service(websocket))
    elif role == "client":
        if websocket.headers.get("x-deskferry-protocol", "").strip() == "2":
            await hub.serve_v2_client(token, websocket, remote, read_resumable(websocket), read_room_proof(websocket), read_service(websocket), heartbeat=read_heartbeat(websocket))
        else:
            await hub.serve_client(token, websocket, remote, read_resumable(websocket), read_room_proof(websocket), read_service(websocket))
    elif role == RESUME_ROLE:
        await hub.serve_resume(token, websocket, remote, websocket.headers.get("x-deskferry-session"), websocket.headers.get("x-deskferry-session-side"), read_room_proof(websocket), read_service(websocket))
    elif role == "home-agent":
        await hub.serve_home_agent(token, websocket, remote, read_room_proof(websocket))
    elif role == "diagnostic-log":
        await hub.serve_diagnostic_log(token, websocket, remote, read_room_proof(websocket), websocket.headers.get("x-deskferry-log-component", ""), websocket.headers.get("x-deskferry-log-instance", ""))
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
        <div class="subtle">DeskFerry Relay v{RELAY_VERSION} · Python WebSocket relay at <code>/relay/ws</code>. Status updates stream live over WebSocket.</div>
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
      const controls = rooms.reduce((sum, r) => sum + (r.control_connections || 0), 0);
      const activePairs = rooms.reduce((sum, r) => sum + (r.active_pairs || 0), 0);
      const homeAgents = rooms.filter(r => r.home_agent_connected).length;
      const homeActiveRooms = rooms.filter(r => r.home_agent_connected || (r.active_pairs || 0) > 0).length;
      setValue(workStatus, controls + waitingAgents + activePairs > 0 ? "Connected" : "Waiting", controls + waitingAgents + activePairs > 0 ? "ok" : "warn");
      workDetail.textContent = `${{controls}} control connections, ${{activePairs}} active sessions.`;
      setValue(homeStatus, homeActiveRooms > 0 ? "Active" : "Waiting", homeActiveRooms > 0 ? "ok" : "warn");
      homeDetail.textContent = `${{homeAgents}} presence socket${{homeAgents === 1 ? "" : "s"}}, ${{activePairs}} active RDP stream${{activePairs === 1 ? "" : "s"}}.`;
      streamStatus.textContent = activePairs.toString();
      streamDetail.textContent = activePairs === 0 ? "No active RDP streams." : `${{activePairs}} RDP stream${{activePairs === 1 ? "" : "s"}} bridged.`;
      if (rooms.length === 0) {{
        roomsBody.innerHTML = '<tr><td colspan="5" class="subtle">No rooms have connected yet.</td></tr>';
        return;
      }}
      roomsBody.innerHTML = rooms.map(r => {{
        const workConnected = (r.control_connections || 0) + (r.waiting_agents || 0) + (r.active_pairs || 0) > 0;
        const homePresence = !!r.home_agent_connected;
        const streamActive = (r.active_pairs || 0) > 0;
        const homeState = homePresence ? "presence" : (streamActive ? "active stream" : "waiting");
        const homeInfo = homePresence
          ? `${{esc(r.home_agent_remote || "")}}<br>${{esc(fmt(r.home_agent_connected_at))}}`
          : `${{r.active_pairs || 0}} active<br>${{esc(fmt(r.last_client_connected_at))}}`;
        return `<tr>
          <td><code>${{esc(r.id)}}</code></td>
          <td>${{pill(workConnected, workConnected ? "connected" : "waiting")}}<br><span class="subtle">${{r.control_connections || 0}} controls<br>${{esc(fmt(r.last_agent_connected_at))}}</span></td>
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
