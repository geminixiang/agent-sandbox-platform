from __future__ import annotations

import base64
import hashlib
import inspect
import re
import uuid
from collections.abc import AsyncIterable, AsyncIterator
from dataclasses import dataclass
from datetime import timedelta
from pathlib import PurePosixPath
from types import TracebackType
from typing import Any, AsyncContextManager, Self, cast

import httpx

from ._credentials import TokenProvider
from ._errors import (
    CommandFailedError,
    SandboxAbortedError,
    SandboxError,
    SandboxIntegrityError,
    SandboxInvalidCursorError,
    SandboxTransferTooLargeError,
    error_from_response,
)
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

    async def list_page(
        self,
        *,
        pool: str | None = None,
        limit: int = 50,
        cursor: str | None = None,
    ) -> SandboxPage:
        if isinstance(limit, bool) or not 1 <= limit <= 100:
            raise ValueError("limit must be an integer between 1 and 100")
        params: dict[str, str | int] = {"limit": limit}
        if pool is not None:
            if not pool.strip():
                raise ValueError("pool must be a non-empty string")
            params["pool"] = pool
        if cursor is not None:
            if not cursor.strip():
                raise ValueError("cursor must be a non-empty string")
            params["cursor"] = cursor
        payload = await self.request("GET", "v1/leases", params=params)
        raw_leases_value = payload.get("leases")
        if not isinstance(raw_leases_value, list):
            raise SandboxError("sandbox platform returned an invalid list response")
        raw_leases = cast(list[object], raw_leases_value)
        next_cursor = payload.get("nextCursor")
        if next_cursor is not None and (not isinstance(next_cursor, str) or not next_cursor):
            raise SandboxError("sandbox platform returned an invalid list response")
        return SandboxPage(
            sandboxes=tuple(Sandbox(self, LeaseRecord.from_dict(_dict(item))) for item in raw_leases),
            next_cursor=next_cursor,
        )

    async def list(self, *, pool: str | None = None, limit: int = 50) -> AsyncIterator[Sandbox]:
        cursor: str | None = None
        seen: set[str] = set()
        while True:
            page = await self.list_page(pool=pool, limit=limit, cursor=cursor)
            if page.next_cursor is not None and page.next_cursor in seen:
                raise SandboxInvalidCursorError(
                    "sandbox platform returned a repeated list cursor",
                    code="INVALID_CURSOR",
                )
            for sandbox in page.sandboxes:
                yield sandbox
            if page.next_cursor is None:
                return
            seen.add(page.next_cursor)
            cursor = page.next_cursor

    async def connect(self, sandbox_id: str) -> Sandbox:
        return await self.get(sandbox_id)

    async def get(self, sandbox_id: str) -> Sandbox:
        payload = await self.request("GET", f"v1/leases/{sandbox_id}")
        return Sandbox(self, LeaseRecord.from_dict(_dict(payload["lease"])))

    async def _authorization_header(self) -> dict[str, str]:
        if self._closed:
            raise RuntimeError("SandboxClient is closed")
        token = self._credentials()
        if inspect.isawaitable(token):
            token = await token
        if not token.strip():
            raise SandboxError("credential provider returned an empty token")
        return {"Authorization": f"Bearer {token}"}

    async def request(self, method: str, path: str, **kwargs: Any) -> dict[str, object]:
        headers = await self._authorization_header()
        try:
            response = await self._http.request(method, path, headers={**headers, **kwargs.pop("headers", {})}, **kwargs)
        except httpx.TimeoutException as error:
            raise SandboxAbortedError("sandbox platform request timed out", code="ABORTED") from error
        except httpx.HTTPError as error:
            raise SandboxError(f"sandbox platform request failed: {error}") from error
        payload = _dict(response.json()) if response.content else {}
        if response.is_error:
            error_payload = _dict(payload.get("error", {}))
            raise error_from_response(status=response.status_code, code=_optional_str(error_payload.get("code")), message=_optional_str(error_payload.get("message")) or f"sandbox platform returned HTTP {response.status_code}")
        return payload


@dataclass(frozen=True, slots=True)
class SandboxPage:
    sandboxes: tuple[Sandbox, ...]
    next_cursor: str | None


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


_MAX_FILE_TRANSFER_BYTES = 64 * 1024 * 1024
_SHA256_HEX = re.compile(r"^[0-9a-f]{64}$")
_CONTENT_DIGEST = re.compile(r"^sha-256=:([A-Za-z0-9+/]{43}=):$")


class FileDownload:
    def __init__(self, client: SandboxClient, path: str) -> None:
        self._client = client
        self._path = path
        self._stream_context: AsyncContextManager[httpx.Response] | None = None
        self._response: httpx.Response | None = None
        self._iterator: AsyncIterator[bytes] | None = None
        self._iterated = False
        self._closed = False
        self._size_bytes: int | None = None
        self._sha256: str | None = None

    @property
    def size_bytes(self) -> int:
        if self._size_bytes is None:
            raise RuntimeError("FileDownload has not been entered")
        return self._size_bytes

    @property
    def sha256(self) -> str:
        if self._sha256 is None:
            raise RuntimeError("FileDownload has not been entered")
        return self._sha256

    async def __aenter__(self) -> Self:
        if self._stream_context is not None:
            raise RuntimeError("FileDownload cannot be entered more than once")
        headers = await self._client._authorization_header()  # pyright: ignore[reportPrivateUsage]
        stream_context = self._client._http.stream(  # pyright: ignore[reportPrivateUsage]
            "GET", self._path, headers={**headers, "Accept": "application/octet-stream"}
        )
        self._stream_context = stream_context
        try:
            response = await stream_context.__aenter__()
        except httpx.TimeoutException as error:
            self._stream_context = None
            self._closed = True
            raise SandboxAbortedError("sandbox platform request timed out", code="ABORTED") from error
        except httpx.HTTPError as error:
            self._stream_context = None
            self._closed = True
            raise SandboxError(f"sandbox platform request failed: {error}") from error
        self._response = response
        try:
            if response.is_error:
                await response.aread()
                raise _error_from_response(response)
            self._size_bytes = _parse_content_length(response.headers.get("Content-Length"))
            self._sha256 = _parse_content_digest(response.headers.get("Content-Digest"))
            content_type = response.headers.get("Content-Type", "").split(";", 1)[0].strip()
            if content_type != "application/octet-stream":
                raise SandboxIntegrityError("sandbox platform returned invalid streaming metadata", code="INVALID_STREAMING_RESPONSE")
            self._iterator = response.aiter_raw()
        except SandboxError:
            await self.close()
            raise
        except httpx.TimeoutException as error:
            await self.close()
            raise SandboxAbortedError("sandbox platform request timed out", code="ABORTED") from error
        except httpx.HTTPError as error:
            await self.close()
            raise SandboxError(f"sandbox platform request failed: {error}") from error
        except BaseException:
            await self.close()
            raise
        return self

    async def __aexit__(self, _type: type[BaseException] | None, _value: BaseException | None, _traceback: TracebackType | None) -> None:
        await self.close()

    def __aiter__(self) -> AsyncIterator[bytes]:
        if self._iterator is None:
            raise RuntimeError("FileDownload must be used as an async context manager")
        if self._iterated:
            raise RuntimeError("FileDownload can only be iterated once")
        self._iterated = True
        return self._iterate()

    async def _iterate(self) -> AsyncIterator[bytes]:
        iterator = self._iterator
        if iterator is None:
            raise RuntimeError("FileDownload must be used as an async context manager")
        digest = hashlib.sha256()
        received = 0
        try:
            async for chunk in iterator:
                received += len(chunk)
                if received > self.size_bytes or received > _MAX_FILE_TRANSFER_BYTES:
                    raise SandboxIntegrityError("streaming download length does not match Content-Length", code="CONTENT_LENGTH_MISMATCH")
                digest.update(chunk)
                yield chunk
        except httpx.TimeoutException as error:
            raise SandboxAbortedError("sandbox platform request timed out", code="ABORTED") from error
        except httpx.HTTPError as error:
            raise SandboxIntegrityError("streaming download ended before normal EOF", code="CONTENT_LENGTH_MISMATCH") from error
        if received != self.size_bytes:
            raise SandboxIntegrityError("streaming download length does not match Content-Length", code="CONTENT_LENGTH_MISMATCH")
        if digest.hexdigest() != self.sha256:
            raise SandboxIntegrityError("streaming download does not match Content-Digest", code="CONTENT_DIGEST_MISMATCH")

    async def close(self) -> None:
        if self._closed:
            return
        self._closed = True
        stream_context = self._stream_context
        self._stream_context = None
        if stream_context is not None:
            await stream_context.__aexit__(None, None, None)


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

    def read_stream(self, path: str | PurePosixPath) -> FileDownload:
        query = httpx.QueryParams({"path": str(path)})
        return FileDownload(self._sandbox.client, f"v1/leases/{self._sandbox.id}/files/content?{query}")

    async def write_stream(
        self,
        path: str | PurePosixPath,
        chunks: AsyncIterable[bytes],
        *,
        size_bytes: int,
        sha256: str,
    ) -> None:
        size_bytes = _require_transfer_size(size_bytes)
        sha256 = _require_sha256(sha256)
        headers = await self._sandbox.client._authorization_header()  # pyright: ignore[reportPrivateUsage]

        async def verified_chunks() -> AsyncIterator[bytes]:
            digest = hashlib.sha256()
            sent = 0
            async for chunk in chunks:
                sent += len(chunk)
                if sent > size_bytes or sent > _MAX_FILE_TRANSFER_BYTES:
                    raise SandboxIntegrityError("streaming upload length does not match size_bytes", code="CONTENT_LENGTH_MISMATCH")
                digest.update(chunk)
                yield chunk
            if sent != size_bytes:
                raise SandboxIntegrityError("streaming upload length does not match size_bytes", code="CONTENT_LENGTH_MISMATCH")
            if digest.hexdigest() != sha256:
                raise SandboxIntegrityError("streaming upload does not match sha256", code="CONTENT_DIGEST_MISMATCH")

        query = httpx.QueryParams({"path": str(path)})
        try:
            response = await self._sandbox.client._http.request(  # pyright: ignore[reportPrivateUsage]
                "PUT",
                f"v1/leases/{self._sandbox.id}/files/content?{query}",
                headers={
                    **headers,
                    "Accept": "application/json",
                    "Content-Type": "application/octet-stream",
                    "Content-Length": str(size_bytes),
                    "Content-Digest": _format_content_digest(sha256),
                },
                content=verified_chunks(),
            )
        except SandboxError:
            raise
        except httpx.TimeoutException as error:
            raise SandboxAbortedError("streaming upload was aborted", code="ABORTED") from error
        except httpx.TransportError as error:
            raise SandboxAbortedError("streaming upload ended before the platform responded", code="ABORTED") from error
        if response.is_error:
            raise _error_from_response(response)

    async def _write(self, path: str | PurePosixPath, content: str, encoding: str) -> None:
        await self._sandbox.client.request("POST", f"v1/leases/{self._sandbox.id}/files/write", json={"path": str(path), "content": content, "encoding": encoding})

    async def _read(self, path: str | PurePosixPath, encoding: str) -> str:
        payload = await self._sandbox.client.request("POST", f"v1/leases/{self._sandbox.id}/files/read", json={"path": str(path), "encoding": encoding})
        return str(payload["content"])


def _require_transfer_size(value: int) -> int:
    if type(value) is not int or value < 0:
        raise ValueError("size_bytes must be a non-negative integer")
    if value > _MAX_FILE_TRANSFER_BYTES:
        raise SandboxTransferTooLargeError("file transfer exceeds the 64 MiB limit", code="TRANSFER_TOO_LARGE")
    return value


def _require_sha256(value: str) -> str:
    if not _SHA256_HEX.fullmatch(value):
        raise ValueError("sha256 must be a lowercase 64-character hexadecimal digest")
    return value


def _format_content_digest(value: str) -> str:
    return f"sha-256=:{base64.b64encode(bytes.fromhex(value)).decode('ascii')}:"


def _parse_content_length(value: str | None) -> int:
    if value is None or not re.fullmatch(r"0|[1-9][0-9]*", value):
        raise SandboxIntegrityError("sandbox platform returned invalid Content-Length", code="INVALID_STREAMING_RESPONSE")
    size = int(value)
    if size > _MAX_FILE_TRANSFER_BYTES:
        raise SandboxTransferTooLargeError("file transfer exceeds the 64 MiB limit", code="TRANSFER_TOO_LARGE")
    return size


def _parse_content_digest(value: str | None) -> str:
    match = _CONTENT_DIGEST.fullmatch(value or "")
    if match is None:
        raise SandboxIntegrityError("sandbox platform returned invalid Content-Digest", code="INVALID_CONTENT_DIGEST")
    try:
        decoded = base64.b64decode(match.group(1), validate=True)
    except ValueError as error:
        raise SandboxIntegrityError("sandbox platform returned invalid Content-Digest", code="INVALID_CONTENT_DIGEST") from error
    if len(decoded) != 32 or base64.b64encode(decoded).decode("ascii") != match.group(1):
        raise SandboxIntegrityError("sandbox platform returned invalid Content-Digest", code="INVALID_CONTENT_DIGEST")
    return decoded.hex()


def _error_from_response(response: httpx.Response) -> SandboxError:
    try:
        payload = _dict(response.json()) if response.content else {}
        error_payload = _dict(payload.get("error", {}))
    except (ValueError, SandboxError):
        error_payload = {}
    return error_from_response(
        status=response.status_code,
        code=_optional_str(error_payload.get("code")),
        message=_optional_str(error_payload.get("message")) or f"sandbox platform returned HTTP {response.status_code}",
    )


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
