from __future__ import annotations

import base64
import inspect
import uuid
from datetime import timedelta
from pathlib import PurePosixPath
from types import TracebackType
from typing import Any, Self, cast

import httpx

from ._credentials import TokenProvider
from ._errors import CommandFailedError, SandboxError, error_from_response
from ._models import CommandResult, LeaseRecord


class SandboxClient:
    def __init__(
        self,
        *,
        base_url: str,
        credentials: TokenProvider,
        timeout: float | httpx.Timeout = 30.0,
        transport: httpx.AsyncBaseTransport | None = None,
    ) -> None:
        self._credentials = credentials
        self._http = httpx.AsyncClient(base_url=base_url.rstrip("/") + "/", timeout=timeout, transport=transport)
        self._closed = False

    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(self, _type: type[BaseException] | None, _value: BaseException | None, _traceback: TracebackType | None) -> None:
        await self.close()

    async def close(self) -> None:
        if not self._closed:
            self._closed = True
            await self._http.aclose()

    def sandbox(
        self,
        *,
        pool: str,
        ttl: timedelta | None = None,
        idempotency_key: str | None = None,
    ) -> SandboxContext:
        return SandboxContext(self, pool=pool, ttl=ttl, idempotency_key=idempotency_key)

    async def create(
        self,
        *,
        pool: str,
        ttl: timedelta | None = None,
        idempotency_key: str | None = None,
    ) -> Sandbox:
        body: dict[str, object] = {"pool": pool}
        if ttl is not None:
            seconds = ttl.total_seconds()
            if not seconds.is_integer() or seconds <= 0:
                raise ValueError("ttl must be a positive whole number of seconds")
            body["ttlSeconds"] = int(seconds)
        payload = await self.request(
            "POST",
            "v1/leases",
            headers={"Idempotency-Key": idempotency_key or str(uuid.uuid4())},
            json=body,
        )
        return Sandbox(self, LeaseRecord.from_dict(_dict(payload["lease"])))

    async def get(self, sandbox_id: str) -> Sandbox:
        payload = await self.request("GET", f"v1/leases/{sandbox_id}")
        return Sandbox(self, LeaseRecord.from_dict(_dict(payload["lease"])))

    async def request(self, method: str, path: str, **kwargs: Any) -> dict[str, object]:
        if self._closed:
            raise RuntimeError("SandboxClient is closed")
        token = self._credentials()
        if inspect.isawaitable(token):
            token = await token
        if not token.strip():
            raise SandboxError("credential provider returned an empty token")
        try:
            response = await self._http.request(method, path, headers={"Authorization": f"Bearer {token}", **kwargs.pop("headers", {})}, **kwargs)
        except httpx.TimeoutException as error:
            raise SandboxError("sandbox platform request timed out") from error
        except httpx.HTTPError as error:
            raise SandboxError(f"sandbox platform request failed: {error}") from error
        payload = _dict(response.json()) if response.content else {}
        if response.is_error:
            error_payload = _dict(payload.get("error", {}))
            raise error_from_response(status=response.status_code, code=_optional_str(error_payload.get("code")), message=_optional_str(error_payload.get("message")) or f"sandbox platform returned HTTP {response.status_code}")
        return payload


class SandboxContext:
    def __init__(self, client: SandboxClient, *, pool: str, ttl: timedelta | None, idempotency_key: str | None) -> None:
        self._client = client
        self._pool = pool
        self._ttl = ttl
        self._idempotency_key = idempotency_key
        self._sandbox: Sandbox | None = None

    async def __aenter__(self) -> Sandbox:
        self._sandbox = await self._client.create(pool=self._pool, ttl=self._ttl, idempotency_key=self._idempotency_key)
        return self._sandbox

    async def __aexit__(self, _type: type[BaseException] | None, _value: BaseException | None, _traceback: TracebackType | None) -> None:
        if self._sandbox is not None:
            await self._sandbox.close()


class Sandbox:
    def __init__(self, client: SandboxClient, record: LeaseRecord) -> None:
        self._client = client
        self.record = record
        self.files = SandboxFiles(self)
        self._closed = False

    @property
    def client(self) -> SandboxClient:
        return self._client

    @property
    def id(self) -> str:
        return self.record.id

    async def run(
        self,
        command: str,
        *,
        cwd: str | PurePosixPath | None = None,
        env: dict[str, str] | None = None,
        timeout: timedelta | None = None,
        check: bool = False,
    ) -> CommandResult:
        body: dict[str, object] = {"command": command}
        if cwd is not None:
            body["cwd"] = str(cwd)
        if env is not None:
            body["env"] = env
        if timeout is not None:
            seconds = timeout.total_seconds()
            if not seconds.is_integer() or seconds <= 0:
                raise ValueError("timeout must be a positive whole number of seconds")
            body["timeoutSeconds"] = int(seconds)
        payload = await self._client.request("POST", f"v1/leases/{self.id}/exec", json=body)
        result = CommandResult(stdout=_required_str(payload, "stdout"), stderr=_required_str(payload, "stderr"), exit_code=_required_int(payload, "code"))
        if check and not result.succeeded:
            raise CommandFailedError(command, result)
        return result

    async def refresh(self) -> LeaseRecord:
        current = await self._client.get(self.id)
        self.record = current.record
        return self.record

    async def release(self) -> LeaseRecord:
        payload = await self._client.request("POST", f"v1/leases/{self.id}/release")
        self.record = LeaseRecord.from_dict(_dict(payload["lease"]))
        self._closed = True
        return self.record

    async def delete(self) -> None:
        await self._client.request("DELETE", f"v1/leases/{self.id}")
        self._closed = True

    async def close(self) -> None:
        if not self._closed:
            await self.release()


class SandboxFiles:
    def __init__(self, sandbox: Sandbox) -> None:
        self._sandbox = sandbox

    async def write_text(self, path: str | PurePosixPath, content: str) -> None:
        await self._write(path, content, "utf8")

    async def read_text(self, path: str | PurePosixPath) -> str:
        return await self._read(path, "utf8")

    async def write_bytes(self, path: str | PurePosixPath, content: bytes) -> None:
        await self._write(path, base64.b64encode(content).decode("ascii"), "base64")

    async def read_bytes(self, path: str | PurePosixPath) -> bytes:
        return base64.b64decode(await self._read(path, "base64"), validate=True)

    async def _write(self, path: str | PurePosixPath, content: str, encoding: str) -> None:
        await self._sandbox.client.request("POST", f"v1/leases/{self._sandbox.id}/files/write", json={"path": str(path), "content": content, "encoding": encoding})

    async def _read(self, path: str | PurePosixPath, encoding: str) -> str:
        payload = await self._sandbox.client.request("POST", f"v1/leases/{self._sandbox.id}/files/read", json={"path": str(path), "encoding": encoding})
        return str(payload["content"])


def _dict(value: object) -> dict[str, object]:
    if not isinstance(value, dict):
        raise SandboxError("sandbox platform returned an invalid response")
    raw = cast(dict[object, object], value)
    return {str(key): item for key, item in raw.items()}


def _required_str(value: dict[str, object], key: str) -> str:
    item = value.get(key)
    if not isinstance(item, str):
        raise SandboxError("sandbox platform returned an invalid response")
    return item


def _required_int(value: dict[str, object], key: str) -> int:
    item = value.get(key)
    if not isinstance(item, int):
        raise SandboxError("sandbox platform returned an invalid response")
    return item


def _optional_str(value: object) -> str | None:
    return value if isinstance(value, str) else None
