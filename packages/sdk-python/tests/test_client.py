from __future__ import annotations

import base64
import hashlib
import json
from collections.abc import AsyncIterator
from dataclasses import FrozenInstanceError
from typing import Any, Callable

import httpx
import pytest

from agent_sandbox import (
    CommandFailedError,
    SandboxAbortedError,
    SandboxClient,
    SandboxCursorExpiredError,
    SandboxIntegrityError,
    SandboxInvalidCursorError,
    SandboxNotFoundError,
    SandboxPage,
    SandboxStreamingNotSupportedError,
    SandboxTransferLimitError,
    SandboxUnknownPoolError,
    StaticToken,
)

RECORD = {
    "id": "lease_1",
    "pool": "coding",
    "status": "active",
    "createdAt": "2026-01-01T00:00:00Z",
    "expiresAt": "2026-01-01T00:15:00Z",
    "lastUsedAt": "2026-01-01T00:00:00Z",
}


@pytest.mark.asyncio
async def test_context_manager_hides_lease_lifecycle_and_supports_files() -> None:
    requests: list[httpx.Request] = []

    async def handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        path = request.url.path
        if path == "/v1/leases":
            return response({"lease": RECORD, "replayed": False}, 201)
        if path.endswith("/files/write"):
            return response({"path": "/workspace/value.bin"})
        if path.endswith("/files/read"):
            return response({"path": "/workspace/value.bin", "content": base64.b64encode(b"value").decode(), "encoding": "base64"})
        if path.endswith("/exec"):
            return response({"stdout": "ok\n", "stderr": "", "code": 0})
        if path.endswith("/release"):
            return response({"lease": {**RECORD, "status": "released"}})
        raise AssertionError(f"unexpected request {request.method} {path}")

    async with SandboxClient(base_url="https://sandbox.example", credentials=StaticToken("subject-token"), transport=httpx.MockTransport(handler)) as client:
        async with client.sandbox(pool="coding", idempotency_key="request-1") as sandbox:
            await sandbox.files.write_bytes("/workspace/value.bin", b"value")
            assert await sandbox.files.read_bytes("/workspace/value.bin") == b"value"
            result = await sandbox.run("echo ok", check=True)
            assert result.stdout == "ok\n"
            assert result.succeeded

    assert requests[0].headers["authorization"] == "Bearer subject-token"
    assert requests[0].headers["idempotency-key"] == "request-1"
    assert json.loads(requests[0].content) == {"pool": "coding"}
    assert requests[-1].url.path.endswith("/release")


class TrackingStream(httpx.AsyncByteStream):
    def __init__(self, chunks: list[bytes]) -> None:
        self.chunks = chunks
        self.yielded = 0
        self.closed = False

    async def __aiter__(self) -> AsyncIterator[bytes]:
        for chunk in self.chunks:
            self.yielded += 1
            yield chunk

    async def aclose(self) -> None:
        self.closed = True


@pytest.mark.asyncio
async def test_stream_download_is_lazy_and_validates_metadata() -> None:
    chunks = [b"first-", b"second"]
    content = b"".join(chunks)
    digest = hashlib.sha256(content).hexdigest()
    stream = TrackingStream(chunks)

    async def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/v1/leases/lease_1":
            return response({"lease": RECORD})
        assert request.url.path.endswith("/files/content")
        assert request.url.params["path"] == "/workspace/data.bin"
        assert request.headers["accept"] == "application/octet-stream"
        return httpx.Response(
            200,
            headers={
                "Content-Type": "application/octet-stream",
                "Content-Length": str(len(content)),
                "Content-Digest": content_digest(digest),
            },
            stream=stream,
        )

    async with SandboxClient(base_url="https://sandbox.example", credentials=StaticToken("subject-token"), transport=httpx.MockTransport(handler)) as client:
        sandbox = await client.get("lease_1")
        async with sandbox.files.read_stream("/workspace/data.bin") as download:
            assert download.size_bytes == len(content)
            assert download.sha256 == digest
            assert stream.yielded == 0
            received = [chunk async for chunk in download]
            assert b"".join(received) == content
        assert stream.closed


class UploadTransport(httpx.AsyncBaseTransport):
    def __init__(self, generated: Callable[[], int], content: bytes, digest: str) -> None:
        self.generated = generated
        self.content = content
        self.digest = digest
        self.received = b""

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        if request.url.path == "/v1/leases/lease_1":
            return response({"lease": RECORD})
        assert request.method == "PUT"
        assert self.generated() == 0
        assert request.headers["content-type"] == "application/octet-stream"
        assert request.headers["content-length"] == str(len(self.content))
        assert request.headers["content-digest"] == content_digest(self.digest)
        self.received = await request.aread()
        return httpx.Response(204)


@pytest.mark.asyncio
async def test_stream_upload_is_lazy_and_sends_wire_headers() -> None:
    content = b"chunk-one|chunk-two"
    digest = hashlib.sha256(content).hexdigest()
    generated = 0

    async def chunks() -> AsyncIterator[bytes]:
        nonlocal generated
        generated += 1
        yield content[:9]
        generated += 1
        yield content[9:]

    transport = UploadTransport(lambda: generated, content, digest)
    async with SandboxClient(base_url="https://sandbox.example", credentials=StaticToken("subject-token"), transport=transport) as client:
        sandbox = await client.get("lease_1")
        await sandbox.files.write_stream("/workspace/data.bin", chunks(), size_bytes=len(content), sha256=digest)
    assert generated == 2
    assert transport.received == content


@pytest.mark.asyncio
async def test_stream_upload_validates_declared_length() -> None:
    content = b"x"
    digest = hashlib.sha256(content).hexdigest()

    async def chunks() -> AsyncIterator[bytes]:
        yield content

    transport = UploadTransport(lambda: 0, b"xx", digest)
    async with SandboxClient(base_url="https://sandbox.example", credentials=StaticToken("subject-token"), transport=transport) as client:
        sandbox = await client.get("lease_1")
        with pytest.raises(SandboxIntegrityError) as captured:
            await sandbox.files.write_stream("/workspace/data.bin", chunks(), size_bytes=2, sha256=digest)
    assert captured.value.code == "CONTENT_LENGTH_MISMATCH"


@pytest.mark.asyncio
async def test_stream_upload_transport_end_is_typed_as_aborted() -> None:
    content = b"x"
    digest = hashlib.sha256(content).hexdigest()

    async def chunks() -> AsyncIterator[bytes]:
        yield content

    async def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/v1/leases/lease_1":
            return response({"lease": RECORD})
        await request.aread()
        raise httpx.ReadError("response ended", request=request)

    async with SandboxClient(base_url="https://sandbox.example", credentials=StaticToken("subject-token"), transport=httpx.MockTransport(handler)) as client:
        sandbox = await client.get("lease_1")
        with pytest.raises(SandboxAbortedError) as captured:
            await sandbox.files.write_stream("/workspace/data.bin", chunks(), size_bytes=1, sha256=digest)
    assert captured.value.code == "ABORTED"


@pytest.mark.asyncio
async def test_stream_download_validates_normal_eof_but_early_close_does_not() -> None:
    digest = hashlib.sha256(b"expected").hexdigest()
    streams = [TrackingStream([b"short"]), TrackingStream([b"one", b"two"])]

    async def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/v1/leases/lease_1":
            return response({"lease": RECORD})
        stream = streams.pop(0)
        return httpx.Response(200, headers={
            "Content-Type": "application/octet-stream",
            "Content-Length": "8",
            "Content-Digest": content_digest(digest),
        }, stream=stream)

    async with SandboxClient(base_url="https://sandbox.example", credentials=StaticToken("subject-token"), transport=httpx.MockTransport(handler)) as client:
        sandbox = await client.get("lease_1")
        with pytest.raises(SandboxIntegrityError) as captured:
            async with sandbox.files.read_stream("/workspace/truncated") as download:
                _ = [chunk async for chunk in download]
        assert captured.value.code == "CONTENT_LENGTH_MISMATCH"

        early_stream = streams[0]
        async with sandbox.files.read_stream("/workspace/early") as download:
            async for _chunk in download:
                break
        assert early_stream.closed


@pytest.mark.asyncio
async def test_streaming_not_supported_is_typed() -> None:
    async def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/v1/leases/lease_1":
            return response({"lease": RECORD})
        return response({"error": {"code": "STREAMING_NOT_SUPPORTED", "message": "not supported"}}, 501)

    async with SandboxClient(base_url="https://sandbox.example", credentials=StaticToken("subject-token"), transport=httpx.MockTransport(handler)) as client:
        sandbox = await client.get("lease_1")
        with pytest.raises(SandboxStreamingNotSupportedError) as captured:
            async with sandbox.files.read_stream("/workspace/data.bin"):
                pass
    assert captured.value.status == 501
    assert captured.value.code == "STREAMING_NOT_SUPPORTED"


@pytest.mark.asyncio
async def test_transfer_concurrency_limit_is_typed() -> None:
    async def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/v1/leases/lease_1":
            return response({"lease": RECORD})
        return response({"error": {"code": "TRANSFER_LIMIT_REACHED", "message": "busy"}}, 429)

    async with SandboxClient(base_url="https://sandbox.example", credentials=StaticToken("subject-token"), transport=httpx.MockTransport(handler)) as client:
        sandbox = await client.get("lease_1")
        with pytest.raises(SandboxTransferLimitError) as captured:
            async with sandbox.files.read_stream("/workspace/data.bin"):
                pass
    assert captured.value.status == 429
    assert captured.value.code == "TRANSFER_LIMIT_REACHED"


@pytest.mark.asyncio
async def test_async_credential_provider_and_typed_error() -> None:
    async def credentials() -> str:
        return "refreshed-token"

    async def handler(request: httpx.Request) -> httpx.Response:
        assert request.headers["authorization"] == "Bearer refreshed-token"
        return response({"error": {"code": "LEASE_NOT_FOUND", "message": "Lease not found"}}, 404)

    async with SandboxClient(base_url="https://sandbox.example", credentials=credentials, transport=httpx.MockTransport(handler)) as client:
        with pytest.raises(SandboxNotFoundError) as captured:
            await client.get("missing")
    assert captured.value.code == "LEASE_NOT_FOUND"
    assert captured.value.status == 404


@pytest.mark.asyncio
async def test_list_page_list_and_connect_handle_empty_pages_with_auth() -> None:
    requests: list[httpx.Request] = []

    async def handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        assert request.headers["authorization"] == "Bearer subject-token"
        if request.url.path == "/v1/leases" and request.url.params.get("cursor") is None:
            assert request.url.params["pool"] == "coding"
            assert request.url.params["limit"] == "1"
            return response({"leases": [], "nextCursor": "page-2"})
        if request.url.path == "/v1/leases" and request.url.params.get("cursor") == "page-2":
            return response({"leases": [RECORD], "nextCursor": None})
        if request.url.path == "/v1/leases/lease_1":
            return response({"lease": RECORD})
        raise AssertionError(f"unexpected request {request.method} {request.url}")

    async with SandboxClient(base_url="https://sandbox.example", credentials=StaticToken("subject-token"), transport=httpx.MockTransport(handler)) as client:
        first = await client.list_page(pool="coding", limit=1)
        assert isinstance(first, SandboxPage)
        assert first.sandboxes == ()
        assert first.next_cursor == "page-2"
        with pytest.raises(FrozenInstanceError):
            setattr(first, "next_cursor", None)
        listed = [sandbox.id async for sandbox in client.list(pool="coding", limit=1)]
        connected = await client.connect("lease_1")

    assert listed == ["lease_1"]
    assert connected.id == "lease_1"
    assert len(requests) == 4


@pytest.mark.asyncio
async def test_list_repeated_cursor_and_typed_errors() -> None:
    async def repeated_handler(_request: httpx.Request) -> httpx.Response:
        return response({"leases": [], "nextCursor": "same"})

    async with SandboxClient(base_url="https://sandbox.example", credentials=StaticToken("token"), transport=httpx.MockTransport(repeated_handler)) as client:
        with pytest.raises(SandboxInvalidCursorError) as repeated:
            _ = [sandbox async for sandbox in client.list(limit=1)]
    assert repeated.value.code == "INVALID_CURSOR"

    cases = [
        ("INVALID_CURSOR", 400, SandboxInvalidCursorError),
        ("CURSOR_EXPIRED", 410, SandboxCursorExpiredError),
        ("UNKNOWN_POOL", 400, SandboxUnknownPoolError),
    ]
    for code, status, error_type in cases:
        async def error_handler(_request: httpx.Request, *, error_code: str = code, error_status: int = status) -> httpx.Response:
            return response({"error": {"code": error_code, "message": error_code}}, error_status)

        async with SandboxClient(base_url="https://sandbox.example", credentials=StaticToken("token"), transport=httpx.MockTransport(error_handler)) as client:
            with pytest.raises(error_type) as captured:
                await client.list_page(cursor="opaque")
        assert captured.value.code == code
        assert captured.value.status == status


@pytest.mark.asyncio
async def test_checked_command_failure_preserves_diagnostics() -> None:
    async def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/v1/leases":
            return response({"lease": RECORD, "replayed": False}, 201)
        if request.url.path.endswith("/exec"):
            return response({"stdout": "partial output\n", "stderr": "traceback\n", "code": 17})
        raise AssertionError(f"unexpected request {request.method} {request.url.path}")

    async with SandboxClient(base_url="https://sandbox.example", credentials=StaticToken("subject-token"), transport=httpx.MockTransport(handler)) as client:
        sandbox = await client.create(pool="coding")
        with pytest.raises(CommandFailedError) as captured:
            await sandbox.run("python broken.py", check=True)

    assert captured.value.command == "python broken.py"
    assert captured.value.result.stdout == "partial output\n"
    assert captured.value.result.stderr == "traceback\n"
    assert captured.value.result.exit_code == 17
    assert captured.value.code == "COMMAND_FAILED"
    assert captured.value.status is None
    assert str(captured.value) == "command exited with status 17"


def content_digest(sha256: str) -> str:
    return f"sha-256=:{base64.b64encode(bytes.fromhex(sha256)).decode('ascii')}:"


def response(body: dict[str, Any], status: int = 200) -> httpx.Response:
    return httpx.Response(status, json=body)
