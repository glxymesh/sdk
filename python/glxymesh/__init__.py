"""What a Python function tool is written against.

A tool is a plain function, ``def run(ctx, input)``; only the gateway speaks
MCP. Its code reaches the internet through ``ctx.fetch``, which goes by the
egress gateway: the tool's allow-list holds there, and its keys are added
there, so the code never holds them.
"""

from __future__ import annotations

import json as _json
import socket
import time
from dataclasses import dataclass
from typing import Any, Callable, Mapping

__all__ = ["Caller", "Ctx", "EgressError", "Logger", "Response", "ToolError", "secret"]


@dataclass(frozen=True)
class Caller:
    """Who the call is for, as code may see them: never an email or a token."""

    org: str
    project: str
    user_id: str
    # The MCP client the person allowed, such as Claude or Cursor.
    client_id: str


class ToolError(Exception):
    """Ends a call with a message the model reads and can act on.

    Anything else raised is treated as a bug: the model gets a fixed sentence
    and the owner the log.
    """


class EgressError(Exception):
    """A request the egress gateway refused or couldn't complete. Its message
    tells the tool's author what to add to tool.yml."""

    def __init__(self, message: str, decision: str) -> None:
        super().__init__(message)
        self.decision = decision


class Logger:
    """Lines ride back with the call's result and show beside the call in the
    console; tool Lambdas write nothing to CloudWatch."""

    def __init__(self, add: Callable[[str, str], None]) -> None:
        self._add = add

    def info(self, message: object) -> None:
        self._add("info", str(message))

    def warn(self, message: object) -> None:
        self._add("warn", str(message))

    def error(self, message: object) -> None:
        self._add("error", str(message))


class Response:
    """An outside service's answer, as the egress gateway passed it on."""

    def __init__(self, url: str, status: int, headers: Mapping[str, str], content: bytes) -> None:
        self.url = url
        self.status = status
        # Header names are lowercase; a repeated header's values are joined
        # with ", ".
        self.headers = dict(headers)
        self.content = content

    @property
    def ok(self) -> bool:
        return 200 <= self.status < 300

    @property
    def text(self) -> str:
        return self.content.decode("utf-8", errors="replace")

    def json(self) -> Any:
        return _json.loads(self.content)

    def raise_for_status(self) -> Response:
        """Raises a ToolError, which the model reads, unless the status is 2xx."""
        if not self.ok:
            body = " ".join(self.text.split())[:300]
            raise ToolError(f"{self.url} answered {self.status}" + (f": {body}" if body else ""))
        return self


class Ctx:
    """The call: who it is for, the tool's way out, its log and its deadline."""

    def __init__(
        self,
        caller: Caller,
        log: Logger,
        deadline: float,
        fetch: Callable[..., Response],
        connect: Callable[[str], Any] | None = None,
        forward: Callable[[str], tuple[str, int]] | None = None,
        secret_value: Callable[[str], str] | None = None,
    ) -> None:
        self.caller = caller
        self.log = log
        self._deadline = deadline
        self._fetch = fetch
        self._connect = connect
        self._forward = forward
        self._secret_value = secret_value

    def remaining(self) -> float:
        """Seconds left before the call's deadline."""
        return max(0.0, self._deadline - time.time())

    def fetch(
        self,
        url: str,
        *,
        method: str = "GET",
        params: Mapping[str, Any] | None = None,
        headers: Mapping[str, str] | None = None,
        json: Any = None,
        data: bytes | str | None = None,
        timeout: float | None = None,
    ) -> Response:
        """Sends a request by way of the egress gateway. A host tool.yml
        doesn't allow raises an EgressError that says what to add.

        ``json`` sends a JSON body; ``data`` sends bytes or text as they are.
        The request gives up at ``timeout`` seconds, or at the call's deadline.
        """
        return self._fetch(
            url, method=method, params=params, headers=headers, json=json, data=data, timeout=timeout
        )

    def connect(self, address: str) -> "socket.socket":
        """A TCP connection, for a database or another protocol that isn't
        HTTPS, by way of the egress gateway: address is host:port as the
        tool's network.connect lists it, or the server a connection string
        names. For a driver that takes a socket, like pg8000's ``sock``. It
        closes when the call ends."""
        if self._connect is None:
            raise EgressError("egress: connections aren't available here", "EGRESS_DECISION_FAILED")
        return self._connect(address)

    def forward(self, address: str) -> tuple[str, int]:
        """Listens on 127.0.0.1 and carries each connection made there to
        address, for a driver that opens its own sockets, like psycopg or
        asyncpg: give it the host and port this returns. It closes when the
        call ends."""
        if self._forward is None:
            raise EgressError("egress: connections aren't available here", "EGRESS_DECISION_FAILED")
        return self._forward(address)

    def secret_value(self, name: str) -> str:
        """A passed-in key's value for this call: a key tool.yml binds with
        mode: pass-in, which a person approved before the revision went
        live. Every other key stays with the egress gateway."""
        if self._secret_value is None:
            raise ToolError(f"{name} can't be read here")
        return self._secret_value(name)


def secret(name: str) -> str:
    """Stands in for a key where no header or query binding fits, such as a
    JSON body. The egress gateway swaps in the value, and only on the hosts the
    key is bound to; anywhere else the request is refused."""
    return "{{secret:" + name + "}}"
