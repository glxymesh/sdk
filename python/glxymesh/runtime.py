"""The project Lambda's side of a call, which the generated router hands its
tools to; a tool author never imports it.

It speaks the JSON forms of the glxymesh/tool/v1 messages (InvokeEvent,
InvokeResult, FetchRequest, FetchResponse), so a bundle carries no protobuf
runtime.
"""

from __future__ import annotations

import asyncio
import base64
import importlib.util
import inspect
import json
import os
import re
import socket
import threading
import sys
import time
import traceback
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any, Callable, Mapping

from . import Caller, Ctx, EgressError, Logger, Response, ToolError

# Past 64 KB a call's log lines are dropped and the result says so.
MAX_LOG_BYTES = 64 << 10
# The egress gateway's own cap; a larger body would be refused there anyway.
MAX_BODY_BYTES = 4 << 20
# No call outlives Lambda's 15 minutes, whatever the event says.
MAX_CALL_SECONDS = 900.0


@dataclass
class Tool:
    run: Callable[..., Any]
    # tool.yml's defaults, for inputs a call leaves out.
    defaults: dict[str, Any]
    # The project's folder, which a crash's traceback names files from.
    root: str = ""


_SDK = os.path.dirname(os.path.abspath(__file__))


def load(root: str, name: str, defaults: dict[str, Any]) -> Tool:
    """Imports tools/<name>/tool.py as a package of its own, so its relative
    imports (from .tool_gen import Input) find the files beside it."""
    folder = os.path.join(root, "tools", name)
    module = "glxy_tools." + re.sub(r"\W", "_", name)
    spec = importlib.util.spec_from_file_location(
        module, os.path.join(folder, "tool.py"), submodule_search_locations=[folder]
    )
    if spec is None or spec.loader is None:
        raise ImportError(f"tools/{name}/tool.py can't be loaded")
    mod = importlib.util.module_from_spec(spec)
    sys.modules[module] = mod
    spec.loader.exec_module(mod)
    run = getattr(mod, "run", None)
    if not callable(run):
        raise ImportError(f"tools/{name}/tool.py has no run(ctx, input) function")
    return Tool(run=run, defaults=defaults, root=root)


class _Lines:
    def __init__(self) -> None:
        self.lines: list[dict[str, str]] = []
        self.size = 0
        self.truncated = False

    def add(self, level: str, message: str) -> None:
        if self.size + len(message) > MAX_LOG_BYTES:
            self.truncated = True
            return
        self.size += len(message)
        self.lines.append({"at": _now(), "level": level, "message": message})


def handler(tools: Mapping[str, Tool], egress_url: str | None = None) -> Callable[[dict, Any], dict]:
    """The Lambda handler the router exports. Every call is answered with a
    result, crashes included, so Lambda never reports a failure of its own for
    a tool's bug."""
    if egress_url is None:
        egress_url = os.environ.get("GLXY_EGRESS_URL", "")

    def handle(event: dict, _context: Any = None) -> dict:
        lines = _Lines()
        tool = tools.get(event.get("tool", ""))
        if tool is None:
            return _crash(f"this build has no tool named {event.get('tool')}", lines)
        deadline = min(_deadline(event.get("deadline")), time.time() + MAX_CALL_SECONDS)
        c = event.get("caller") or {}
        caller = Caller(
            org=c.get("org", ""),
            project=c.get("project", ""),
            user_id=c.get("userId", ""),
            client_id=c.get("clientId", ""),
        )
        token = event.get("egressToken", "")

        def fetch(url: str, **kw: Any) -> Response:
            return _egress_fetch(egress_url, token, deadline, url, **kw)

        opened: list[Any] = []

        def connect(address: str) -> socket.socket:
            s = _egress_connect(egress_url, token, deadline, address)
            opened.append(s)
            return s

        def forward(address: str) -> tuple[str, int]:
            return _forward(lambda: connect(address), opened, lines)

        def secret_value(name: str) -> str:
            return _egress_secret(egress_url, token, deadline, name)

        ctx = Ctx(caller, Logger(lines.add), deadline, fetch, connect, forward, secret_value)
        try:
            return _call(tool, ctx, event, lines)
        finally:
            for o in opened:
                try:
                    o.close()
                except OSError:
                    pass

    return handle


def _call(tool: Tool, ctx: Ctx, event: dict, lines: _Lines) -> dict:
    try:
        given = json.loads(event["argumentsJson"]) if event.get("argumentsJson") else {}
        result = tool.run(ctx, {**tool.defaults, **(given or {})})
        if inspect.isawaitable(result):
            result = asyncio.run(_awaited(result))
        try:
            out = {"resultJson": json.dumps(result)}
        except (TypeError, ValueError):
            return _crash("the result isn't JSON", lines)
    except ToolError as err:
        out = {"errorKind": "ERROR_KIND_TOOL", "errorMessage": str(err)}
    except Exception as err:  # noqa: BLE001 - a tool's bug is reported, never raised to Lambda
        lines.add("error", _trace(err, tool.root))
        return _crash(str(err) or type(err).__name__, lines)
    return {**out, "logs": lines.lines, "logsTruncated": lines.truncated}


async def _awaited(result: Any) -> Any:
    return await result


def _trace(err: BaseException, root: str) -> str:
    """The traceback from the tool's own frames, the SDK's left out, with
    files named from the project's folder."""
    frames = [f for f in traceback.extract_tb(err.__traceback__) if not f.filename.startswith(_SDK + os.sep)]
    text = "Traceback (most recent call last):\n" + "".join(traceback.format_list(frames))
    text += "".join(traceback.format_exception_only(type(err), err))
    return text.replace(root + os.sep, "") if root else text


def _crash(message: str, lines: _Lines) -> dict:
    return {
        "errorKind": "ERROR_KIND_CRASH",
        "errorMessage": message,
        "logs": lines.lines,
        "logsTruncated": lines.truncated,
    }


def _egress_fetch(
    egress_url: str,
    token: str,
    deadline: float,
    url: str,
    *,
    method: str = "GET",
    params: Mapping[str, Any] | None = None,
    headers: Mapping[str, str] | None = None,
    json: Any = None,
    data: bytes | str | None = None,
    timeout: float | None = None,
) -> Response:
    """Every outside request goes to the egress gateway's /fetch with the
    call's token: a tool Lambda's network has no other route, and its DNS
    answers nothing else."""
    if not egress_url:
        raise EgressError("egress: no egress gateway is configured for this call", "EGRESS_DECISION_FAILED")
    if params:
        query = urllib.parse.urlencode({k: v for k, v in params.items() if v is not None}, doseq=True)
        url += ("&" if urllib.parse.urlsplit(url).query else "?") + query
    sent = {k.lower(): str(v) for k, v in (headers or {}).items()}
    body: bytes | None = None
    if json is not None:
        body = _dumps(json)
        sent.setdefault("content-type", "application/json")
    elif data is not None:
        body = data.encode() if isinstance(data, str) else bytes(data)
    if body is not None and len(body) > MAX_BODY_BYTES:
        raise EgressError("egress: request body over 4 MB", "EGRESS_DECISION_TOO_LARGE")
    fr: dict[str, Any] = {
        "method": method.upper(),
        "url": url,
        "headers": [{"name": k, "value": v} for k, v in sent.items()],
    }
    if body:
        fr["body"] = base64.b64encode(body).decode()
    left = max(0.001, deadline - time.time())
    req = urllib.request.Request(
        egress_url + "/fetch",
        data=_dumps(fr),
        method="POST",
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=min(timeout or left, left)) as res:
            got = _loads(res.read())
    except urllib.error.HTTPError as err:
        raise RuntimeError(f"egress: the egress gateway answered {err.code}") from None
    if not got.get("status"):
        raise EgressError(f"egress: {got.get('error', 'refused')}", got.get("decision", "EGRESS_DECISION_FAILED"))
    joined: dict[str, str] = {}
    for h in got.get("headers", []):
        name = h.get("name", "").lower()
        joined[name] = f"{joined[name]}, {h.get('value', '')}" if name in joined else h.get("value", "")
    content = base64.b64decode(got["body"]) if got.get("body") else b""
    return Response(url, int(got["status"]), joined, content)


def _egress_connect(egress_url: str, token: str, deadline: float, address: str) -> socket.socket:
    """Upgrades a connection to the egress gateway's /connect into a TCP
    connection to address; what follows the gateway's 101 is the upstream's
    bytes, so the answer is read a byte at a time to leave them unread."""
    if not egress_url:
        raise EgressError("egress: no egress gateway is configured for this call", "EGRESS_DECISION_FAILED")
    u = urllib.parse.urlsplit(egress_url)
    s = socket.create_connection((u.hostname, u.port or 80), timeout=max(0.001, deadline - time.time()))
    s.sendall(
        (
            f"GET /connect HTTP/1.1\r\nHost: {u.netloc}\r\nConnection: Upgrade\r\nUpgrade: glxymesh-tcp\r\n"
            f"Authorization: Bearer {token}\r\nX-Glxymesh-Connect: {address}\r\n\r\n"
        ).encode()
    )
    head = b""
    while not head.endswith(b"\r\n\r\n"):
        b = s.recv(1)
        if not b or len(head) > 16 << 10:
            s.close()
            raise EgressError("egress: the egress gateway sent no answer", "EGRESS_DECISION_FAILED")
        head += b
    status = int(head.split(b" ", 2)[1])
    if status != 101:
        length = re.search(rb"(?i)content-length: *(\d+)", head)
        body = b""
        if length:
            want = int(length.group(1))
            while len(body) < want:
                chunk = s.recv(want - len(body))
                if not chunk:
                    break
                body += chunk
        s.close()
        why = body.decode(errors="replace").strip()
        try:
            why = json.loads(why).get("error", why)
        except ValueError:
            pass
        raise EgressError(f"egress: {why or f'the egress gateway answered {status}'}", "EGRESS_DECISION_HOST_NOT_ALLOWED")
    s.settimeout(max(0.001, deadline - time.time()))
    return s


def _forward(dial: Callable[[], socket.socket], opened: list[Any], lines: _Lines) -> tuple[str, int]:
    ln = socket.create_server(("127.0.0.1", 0))
    opened.append(ln)

    def pump(a: socket.socket, b: socket.socket) -> None:
        try:
            while chunk := a.recv(65536):
                b.sendall(chunk)
        except OSError:
            pass
        finally:
            try:
                b.shutdown(socket.SHUT_WR)
            except OSError:
                pass

    def accept() -> None:
        while True:
            try:
                local, _ = ln.accept()
            except OSError:
                return
            opened.append(local)
            try:
                remote = dial()
            except Exception as err:  # noqa: BLE001 - reported to the owner, the driver sees a closed socket
                lines.add("error", f"forward: {err}")
                local.close()
                continue
            threading.Thread(target=pump, args=(local, remote), daemon=True).start()
            threading.Thread(target=pump, args=(remote, local), daemon=True).start()

    threading.Thread(target=accept, daemon=True).start()
    host, port = ln.getsockname()[:2]
    return host, port


def _egress_secret(egress_url: str, token: str, deadline: float, name: str) -> str:
    if not egress_url:
        raise EgressError("egress: no egress gateway is configured for this call", "EGRESS_DECISION_FAILED")
    req = urllib.request.Request(
        f"{egress_url}/secret/{urllib.parse.quote(name)}", headers={"Authorization": f"Bearer {token}"}
    )
    try:
        with urllib.request.urlopen(req, timeout=max(0.001, deadline - time.time())) as res:
            return _loads(res.read()).get("value", "")
    except urllib.error.HTTPError as err:
        try:
            why = _loads(err.read()).get("error", "")
        except ValueError:
            why = ""
        raise ToolError(why or f"the egress gateway answered {err.code}") from None


def serve(handle: Callable[[dict, Any], dict], api: str) -> None:
    """Takes calls from a Lambda Runtime API until the process ends. In Lambda
    the Python runtime does this itself; gxmesh dev runs the router with
    GLXY_RUNTIME_API pointing at its own stand-in."""
    base = f"http://{api}/2018-06-01/runtime/invocation/"
    while True:
        with urllib.request.urlopen(base + "next") as nxt:
            rid = nxt.headers.get("Lambda-Runtime-Aws-Request-Id", "")
            event = _loads(nxt.read())
        result = handle(event, None)
        req = urllib.request.Request(
            f"{base}{rid}/response", data=_dumps(result), method="POST", headers={"Content-Type": "application/json"}
        )
        urllib.request.urlopen(req).close()


def _deadline(raw: str | None) -> float:
    if not raw:
        return time.time() + 10.0
    # A Timestamp's JSON form carries up to nine fraction digits; datetime
    # reads six.
    raw = re.sub(r"(\.\d{6})\d+", r"\1", raw).replace("Z", "+00:00")
    return datetime.fromisoformat(raw).timestamp()


def _now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def _dumps(v: Any) -> bytes:
    return json.dumps(v, separators=(",", ":")).encode()


def _loads(b: bytes) -> Any:
    return json.loads(b)
