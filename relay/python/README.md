# DeskFerry Python Relay

This is a Python/FastAPI implementation of the DeskFerry Azure WebSocket relay.

It matches the .NET relay contract:

- `GET /relay/` and `GET /relay/<room>` dashboard UI.
- `GET /relay/health` health JSON.
- `GET /relay/status?room=<room>` status JSON.
- `GET /relay/ws` and `GET /relay/<room>/ws` WebSocket endpoints.
- WebSocket roles through `X-DeskFerry-Role`: `agent-control`, `agent-session`, `client`, `resume`, legacy `agent`, `home-agent`, `diagnostic-log`, `probe`, and `dashboard`.
- Typed protocol-v2 session offers, immediate busy/no-agent results, on-demand data connections, and legacy rollback compatibility.

Run locally:

```powershell
python -m pip install -r relay\python\requirements.txt
python -m uvicorn app:app --app-dir relay\python --host 127.0.0.1 --port 8000
```

Use this relay URL for a local room:

```text
http://127.0.0.1:8000/relay/workdesk
```

Build the deployable zips:

```powershell
python -m pip install -r relay\python\requirements-dev.txt
.\build\build-python-relay.ps1
```

The build writes:

```text
dist\python-relay\deskferry-python-relay.zip
dist\python-relay\deskferry-python-relay-linux-cp39-vendored.zip
```

The vendored zip targets Oracle Linux 9's Python 3.9 runtime and includes FastAPI, Uvicorn, and compatibility backports under `vendor/`.

## Linux VM Deployment

The active OCI VM deployment now uses the Go relay under `relay/go/`. This Python relay remains available for Python-capable hosts or local testing. A Python Linux VM deployment would use the same room URL shape:

```text
http://217.142.228.117/relay/b
```

This deployment listens on plain HTTP port `80`. Configure clients with `http://217.142.228.117/relay/<room>`, not `https://217.142.228.117/relay/<room>`, because the VM does not serve the relay on port `443`.

The VM runs the relay directly on port `80` as:

```text
deskferry-relay.service
```

Example service layout:

```text
/opt/deskferry/python-relay/app.py
/opt/deskferry/python-relay/vendor/
/etc/systemd/system/deskferry-relay.service
```

The systemd service uses:

```text
PYTHONPATH=/opt/deskferry/python-relay/vendor
/usr/bin/python3 -m uvicorn app:app --host 0.0.0.0 --port 80 --proxy-headers
```

The VM is configured with a 2 GiB swap file, persistent journald, `softdog` plus systemd `RuntimeWatchdogSec=60s`, and kernel panic recovery for hung tasks. The `deskferry-relay-healthcheck.timer` unit checks `http://127.0.0.1/relay/health` every minute, restarts `deskferry-relay.service` when the local health endpoint fails, and reboots the VM after three consecutive failed post-restart checks.

Useful checks on the VM:

```sh
systemctl status deskferry-relay
systemctl status deskferry-relay-healthcheck.timer
systemctl show --property=RuntimeWatchdogUSec --property=RebootWatchdogUSec
free -h
swapon --show
curl -fsS http://127.0.0.1/relay/health
curl -fsS 'http://127.0.0.1/relay/status?room=b'
```

The OCI network and guest firewall must allow inbound TCP `80`. SSH access from this workstation used HTTP CONNECT through `192.9.200.25:3128`.

## Diagnostics And Retention

The relay logs WebSocket roles, room and pairing lifecycle, bridge direction, bytes and message counts, duration, close details, socket state, cancellation state, and errors through standard Python/ASGI logging. Tunnel payload contents and credentials are not intentionally logged.

Log retention belongs to the hosting process supervisor or platform. A systemd deployment can use persistent journald with a `MaxRetentionSec=7day` drop-in and a disk-usage cap; an App Service deployment should use the equivalent App Service diagnostic settings.
