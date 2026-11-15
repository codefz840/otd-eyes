#!/usr/bin/env python3
"""OpenTabletDriver -> OBS Browser Source bridge for the tablet eyes.

Connects to the running OTD daemon (JSON-RPC over its Unix socket), turns on
tablet debug reports, converts each pen position to a 0..1 position inside the
tablet area configured in OTD, and pushes it to every browser connected on
ws://HOST:PORT/ws. It also serves eyes.html at http://HOST:PORT/.

No third-party dependencies; Python 3.9+.
"""
import argparse
import asyncio
import base64
import hashlib
import json
import math
import os
import signal
import struct
import time

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_SOCK = "/tmp/CoreFxPipe_OpenTabletDriver.Daemon"


def log(*a):
    print(time.strftime("%H:%M:%S"), *a, flush=True)


# --------------------------------------------------------------------------- #
# WebSocket / HTTP (minimal, server side only)
# --------------------------------------------------------------------------- #
class Hub:
    def __init__(self):
        self.clients = set()
        self.last = None  # last message, replayed to new clients

    def broadcast(self, msg: dict):
        data = json.dumps(msg, separators=(",", ":")).encode()
        self.last = data
        frame = ws_frame(data)
        for w in list(self.clients):
            if w.is_closing():
                self.clients.discard(w)
                continue
            # Drop frames for a client that is not keeping up instead of queueing forever.
            if w.transport.get_write_buffer_size() > 256 * 1024:
                continue
            w.write(frame)


def ws_frame(payload: bytes, opcode=0x1) -> bytes:
    n = len(payload)
    if n < 126:
        head = struct.pack("!BB", 0x80 | opcode, n)
    elif n < 65536:
        head = struct.pack("!BBH", 0x80 | opcode, 126, n)
    else:
        head = struct.pack("!BBQ", 0x80 | opcode, 127, n)
    return head + payload


CONTENT_TYPES = {".html": "text/html; charset=utf-8", ".png": "image/png",
                 ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml"}


async def handle_http(reader, writer, hub: Hub):
    try:
        req = await asyncio.wait_for(reader.readuntil(b"\r\n\r\n"), 10)
    except Exception:
        writer.close()
        return
    lines = req.decode("latin1").split("\r\n")
    try:
        method, target, _ = lines[0].split(" ", 2)
    except ValueError:
        writer.close()
        return
    headers = {}
    for line in lines[1:]:
        if ":" in line:
            k, v = line.split(":", 1)
            headers[k.strip().lower()] = v.strip()
    path = target.split("?", 1)[0]

    if path == "/ws" and headers.get("upgrade", "").lower() == "websocket":
        key = headers.get("sec-websocket-key", "")
        accept = base64.b64encode(hashlib.sha1(
            (key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
        writer.write((
            "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"
            f"Connection: Upgrade\r\nSec-WebSocket-Accept: {accept}\r\n\r\n").encode())
        hub.clients.add(writer)
        if hub.last:
            writer.write(ws_frame(hub.last))
        try:
            await ws_drain_incoming(reader, writer)
        except (OSError, asyncio.IncompleteReadError):
            pass  # browser went away
        finally:
            hub.clients.discard(writer)
            writer.close()
        return

    if path == "/":
        path = "/eyes.html"
    name = os.path.basename(path)
    fpath = os.path.join(HERE, name)
    ext = os.path.splitext(name)[1].lower()
    if method != "GET" or ext not in CONTENT_TYPES or not os.path.isfile(fpath):
        writer.write(b"HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
    else:
        with open(fpath, "rb") as f:
            body = f.read()
        writer.write((f"HTTP/1.1 200 OK\r\nContent-Type: {CONTENT_TYPES[ext]}\r\n"
                      f"Content-Length: {len(body)}\r\nCache-Control: no-cache\r\n"
                      "Connection: close\r\n\r\n").encode() + body)
    try:
        await writer.drain()
    finally:
        writer.close()


async def ws_drain_incoming(reader, writer):
    """Read (and ignore) client frames; answer pings, stop on close."""
    while True:
        h = await reader.readexactly(2)
        opcode, n = h[0] & 0x0F, h[1] & 0x7F
        if n == 126:
            n = struct.unpack("!H", await reader.readexactly(2))[0]
        elif n == 127:
            n = struct.unpack("!Q", await reader.readexactly(8))[0]
        mask = await reader.readexactly(4) if h[1] & 0x80 else b""
        payload = await reader.readexactly(n)
        if mask:
            payload = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        if opcode == 0x8:
            writer.write(ws_frame(b"", 0x8))
            return
        if opcode == 0x9:
            writer.write(ws_frame(payload, 0xA))


# --------------------------------------------------------------------------- #
# OpenTabletDriver daemon client (StreamJsonRpc: Content-Length framed JSON-RPC)
# --------------------------------------------------------------------------- #
class OtdClient:
    def __init__(self, sock_path, hub: Hub, rate_hz: float):
        self.sock_path = sock_path
        self.hub = hub
        self.min_dt = 1.0 / rate_hz if rate_hz > 0 else 0
        self.next_id = 0
        self.pending = {}
        self.writer = None
        self.specs = {}   # tablet name -> digitizer spec dict
        self.areas = {}   # tablet name -> tablet area dict (mm, centre based)
        self.last_send = 0.0
        self.in_range = False
        self.warned_paths = set()

    def _send(self, obj):
        body = json.dumps(obj).encode()
        self.writer.write(b"Content-Length: %d\r\n\r\n" % len(body) + body)

    async def call(self, method, *params):
        self.next_id += 1
        fut = asyncio.get_running_loop().create_future()
        self.pending[self.next_id] = fut
        self._send({"jsonrpc": "2.0", "id": self.next_id, "method": method, "params": list(params)})
        return await asyncio.wait_for(fut, 10)

    async def _read_msg(self, reader):
        length = None
        while True:
            line = await reader.readline()
            if not line:
                raise ConnectionError("daemon closed the connection")
            line = line.strip()
            if not line:
                break
            k, _, v = line.decode().partition(":")
            if k.strip().lower() == "content-length":
                length = int(v)
        return json.loads(await reader.readexactly(length))

    async def run_forever(self):
        while True:
            try:
                await self._session()
            except (OSError, ConnectionError, RuntimeError, ValueError, asyncio.IncompleteReadError, asyncio.TimeoutError) as e:
                log(f"OTD: {e.__class__.__name__}: {e} — retrying in 2s")
            self.hub.broadcast({"t": "status", "otd": False})
            await asyncio.sleep(2)

    async def _session(self):
        reader, self.writer = await asyncio.open_unix_connection(self.sock_path)
        self.pending.clear()
        reader_task = asyncio.create_task(self._reader(reader))
        try:
            await self.refresh()
            await self.call("SetTabletDebug", True)
            log("OTD: connected, tablet debug reports enabled")
            self.hub.broadcast({"t": "status", "otd": True})
            while not reader_task.done():
                # Something else (e.g. closing the OTD GUI tablet debugger) can turn
                # debug reports off again, and settings may change: re-assert both.
                await asyncio.wait({reader_task}, timeout=3)
                if not reader_task.done():
                    await self.call("SetTabletDebug", True)
                    await self.refresh()
            reader_task.result()
        finally:
            reader_task.cancel()
            self.writer.close()

    async def refresh(self):
        tablets = await self.call("GetTablets") or []
        for t in tablets:
            props = t.get("Properties") or {}
            dig = (props.get("Specifications") or {}).get("Digitizer")
            if props.get("Name") and dig:
                self.specs[props["Name"]] = dig
        settings = await self.call("GetSettings") or {}
        for p in settings.get("Profiles") or []:
            area = (p.get("AbsoluteModeSettings") or {}).get("Tablet")
            if p.get("Tablet") and area:
                self.areas[p["Tablet"]] = area

    async def _reader(self, reader):
        while True:
            msg = await self._read_msg(reader)
            if "id" in msg and "method" not in msg:
                fut = self.pending.pop(msg["id"], None)
                if fut and not fut.done():
                    if "error" in msg:
                        fut.set_exception(RuntimeError(json.dumps(msg["error"])))
                    else:
                        fut.set_result(msg.get("result"))
            elif msg.get("method") == "DeviceReport":
                params = msg.get("params") or []
                if params:
                    self.on_report(params[-1])

    def on_report(self, rep):
        tablet = ((rep.get("Tablet") or {}).get("Properties") or {}).get("Name")
        path = rep.get("Path") or ""
        data = rep.get("Data") or {}
        pos = data.get("Position") if isinstance(data, dict) else None

        if "OutOfRange" in path:
            if self.in_range:
                self.in_range = False
                self.hub.broadcast({"t": "out"})
            return
        if not isinstance(pos, dict) or "X" not in pos:
            if path not in self.warned_paths:  # aux buttons etc.; log once
                self.warned_paths.add(path)
                log(f"OTD: ignoring report type {path}")
            return

        now = time.monotonic()
        if self.in_range and now - self.last_send < self.min_dt:
            return
        uv = self.normalize(tablet, pos["X"], pos["Y"])
        if uv is None:
            return
        self.last_send = now
        self.in_range = True
        msg = {"t": "pos", "u": round(uv[0], 5), "v": round(uv[1], 5)}
        if "Pressure" in data:
            msg["p"] = data["Pressure"]
        self.hub.broadcast(msg)

    def normalize(self, tablet, x, y):
        """Raw digitizer units -> 0..1 inside the OTD tablet area (may exceed 0..1)."""
        dig = self.specs.get(tablet)
        if not dig and len(self.specs) == 1:
            dig = next(iter(self.specs.values()))
        if not dig or not dig.get("MaxX") or not dig.get("MaxY"):
            return None
        mm_x = x / dig["MaxX"] * dig["Width"]
        mm_y = y / dig["MaxY"] * dig["Height"]
        area = self.areas.get(tablet) or {
            "Width": dig["Width"], "Height": dig["Height"],
            "X": dig["Width"] / 2, "Y": dig["Height"] / 2, "Rotation": 0}
        dx, dy = mm_x - area["X"], mm_y - area["Y"]
        rot = math.radians(area.get("Rotation") or 0)
        if rot:
            c, s = math.cos(-rot), math.sin(-rot)
            dx, dy = dx * c - dy * s, dx * s + dy * c
        return dx / area["Width"] + 0.5, dy / area["Height"] + 0.5

    async def disable_debug(self):
        if self.writer and not self.writer.is_closing():
            try:
                await asyncio.wait_for(self.call("SetTabletDebug", False), 1)
            except Exception:
                pass


async def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=8765)
    ap.add_argument("--socket", default=DEFAULT_SOCK, help="OTD daemon socket path")
    ap.add_argument("--rate", type=float, default=500, help="max position messages per second (0 = no limit)")
    args = ap.parse_args()

    hub = Hub()
    otd = OtdClient(args.socket, hub, args.rate)
    server = await asyncio.start_server(lambda r, w: handle_http(r, w, hub), args.host, args.port)
    log(f"Eyes page: http://{args.host}:{args.port}/   (add this as an OBS Browser Source)")

    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, stop.set)
    otd_task = asyncio.create_task(otd.run_forever())
    await stop.wait()
    await otd.disable_debug()
    otd_task.cancel()
    server.close()
    log("bye")


if __name__ == "__main__":
    asyncio.run(main())
