from __future__ import annotations

import asyncio
import base64
import hashlib
import hmac
import json
import os
import time
from collections.abc import AsyncIterator, Awaitable
from dataclasses import dataclass, field, replace
from typing import Protocol

import httpx
from agent_sandbox import (
    Sandbox,
    SandboxAbortedError,
    SandboxClient,
    SandboxError,
)

CHUNK_BYTES = 64 * 1024
ROUND_TRIP_BYTES = 32 * 1024 * 1024
PAYLOAD_BLOCK = hashlib.sha256(b"agent-sandbox-python-streaming-e2e").digest()


class Digest(Protocol):
    def update(self, value: bytes, /) -> None: ...
    def hexdigest(self) -> str: ...


@dataclass(slots=True)
class Integrity:
    count: int = 0
    digest: Digest = field(default_factory=hashlib.sha256)

    def update(self, chunk: bytes) -> None:
        self.count += len(chunk)
        self.digest.update(chunk)

    def hexdigest(self) -> str:
        return self.digest.hexdigest()


def payload_chunk(size: int) -> bytes:
    return (PAYLOAD_BLOCK * ((size + len(PAYLOAD_BLOCK) - 1) // len(PAYLOAD_BLOCK)))[:size]


def expected_digest(size: int) -> str:
    digest = hashlib.sha256()
    remaining = size
    while remaining:
        length = min(CHUNK_BYTES, remaining)
        digest.update(payload_chunk(length))
        remaining -= length
    return digest.hexdigest()


async def chunks(
    size: int,
    integrity: Integrity,
    *,
    first_chunk: asyncio.Event | None = None,
    continue_after_first: asyncio.Event | None = None,
    delay: float = 0,
) -> AsyncIterator[bytes]:
    remaining = size
    sequence = 0
    while remaining:
        value = payload_chunk(min(CHUNK_BYTES, remaining))
        integrity.update(value)
        yield value
        remaining -= len(value)
        if sequence == 0 and first_chunk is not None:
            first_chunk.set()
            if continue_after_first is not None:
                await continue_after_first.wait()
        elif delay:
            await asyncio.sleep(delay)
        sequence += 1


async def one_chunk(value: bytes = b"x") -> AsyncIterator[bytes]:
    yield value


def subject_token(subject_id: str) -> str:
    claims = json.dumps(
        {
            "consumerId": required("SANDBOX_TEST_CONSUMER_ID"),
            "subjectId": subject_id,
            "exp": int(time.time()) + 300,
        },
        separators=(",", ":"),
    ).encode()
    payload = base64.urlsafe_b64encode(claims).decode().rstrip("=")
    signed = f"v1.{payload}"
    signature = base64.urlsafe_b64encode(
        hmac.new(required("SANDBOX_TEST_CONSUMER_SECRET").encode(), signed.encode(), hashlib.sha256).digest()
    ).decode().rstrip("=")
    return f"{signed}.{signature}"


async def expect_error(awaitable: Awaitable[object], *, status: int, code: str) -> SandboxError:
    try:
        await awaitable
    except SandboxError as error:
        assert (error.status, error.code) == (status, code), (error.status, error.code)
        return error
    raise AssertionError(f"expected {status} {code}")


async def expect_download_error(sandbox: Sandbox, path: str, *, status: int, code: str) -> SandboxError:
    try:
        async with sandbox.files.read_stream(path):
            pass
    except SandboxError as error:
        assert (error.status, error.code) == (status, code), (error.status, error.code)
        return error
    raise AssertionError(f"expected download error {status} {code}")


async def verify_download(sandbox: Sandbox, path: str, size: int, digest: str) -> None:
    integrity = Integrity()
    pending = bytearray()
    async with sandbox.files.read_stream(path) as download:
        assert (download.size_bytes, download.sha256) == (size, digest)
        async for received in download:
            pending.extend(received)
            while len(pending) >= CHUNK_BYTES:
                integrity.update(bytes(pending[:CHUNK_BYTES]))
                del pending[:CHUNK_BYTES]
        if pending:
            integrity.update(bytes(pending))
    assert (integrity.count, integrity.hexdigest()) == (size, digest)


async def malformed_digest_upload(token: str, sandbox: Sandbox, path: str) -> None:
    original_size = ROUND_TRIP_BYTES
    original_digest = expected_digest(original_size)
    content = b"digest-mismatch"
    wrong_digest = hashlib.sha256(b"not-the-request-body").digest()
    digest_header = base64.b64encode(wrong_digest).decode()
    async with httpx.AsyncClient(base_url=required("SANDBOX_PLATFORM_URL"), timeout=30) as client:
        response = await client.put(
            f"/v1/leases/{sandbox.id}/files/content",
            params={"path": path},
            headers={
                "Authorization": f"Bearer {token}",
                "Content-Type": "application/octet-stream",
                "Content-Length": str(len(content)),
                "Content-Digest": f"sha-256=:{digest_header}:",
            },
            content=content,
        )
    assert response.status_code == 422, response.text
    payload = response.json()
    assert payload["error"]["code"] == "CONTENT_DIGEST_MISMATCH", payload
    await verify_download(sandbox, path, original_size, original_digest)


async def verify_subject_scoping(owner: Sandbox, other: SandboxClient) -> None:
    unknown_id = "00000000-0000-4000-8000-000000000000"
    unknown = await expect_error(other.get(unknown_id), status=404, code="LEASE_NOT_FOUND")
    foreign = await expect_error(other.get(owner.id), status=404, code="LEASE_NOT_FOUND")
    assert (foreign.status, foreign.code) == (unknown.status, unknown.code)

    foreign_sandbox = Sandbox(other, owner.record)
    unknown_sandbox = Sandbox(other, replace(owner.record, id=unknown_id))
    unknown_read = await expect_download_error(
        unknown_sandbox, "/workspace/round-trip.bin", status=404, code="LEASE_NOT_FOUND"
    )
    foreign_read = await expect_download_error(
        foreign_sandbox, "/workspace/round-trip.bin", status=404, code="LEASE_NOT_FOUND"
    )
    unknown_write = await expect_error(
        unknown_sandbox.files.write_stream(
            "/workspace/foreign.bin", one_chunk(), size_bytes=1, sha256=hashlib.sha256(b"x").hexdigest()
        ),
        status=404,
        code="LEASE_NOT_FOUND",
    )
    foreign_write = await expect_error(
        foreign_sandbox.files.write_stream(
            "/workspace/foreign.bin", one_chunk(), size_bytes=1, sha256=hashlib.sha256(b"x").hexdigest()
        ),
        status=404,
        code="LEASE_NOT_FOUND",
    )
    assert (foreign_read.status, foreign_read.code) == (unknown_read.status, unknown_read.code)
    assert (foreign_write.status, foreign_write.code) == (unknown_write.status, unknown_write.code)


async def verify_symlink_rejection(sandbox: Sandbox) -> None:
    sentinel = "/dev/shm/agent-sandbox-streaming-sentinel"
    await sandbox.run(
        f"printf outside > {sentinel}; rm -f /workspace/outside-link; ln -s {sentinel} /workspace/outside-link",
        check=True,
    )
    await expect_download_error(sandbox, "/workspace/outside-link", status=400, code="INVALID_PATH")
    await expect_error(
        sandbox.files.write_stream(
            "/workspace/outside-link", one_chunk(), size_bytes=1, sha256=hashlib.sha256(b"x").hexdigest()
        ),
        status=400,
        code="INVALID_PATH",
    )
    result = await sandbox.run(f"cat {sentinel}", check=True)
    assert result.stdout == "outside"


async def verify_pool(pool: str, owner: SandboxClient, other: SandboxClient) -> dict[str, object]:
    sandbox = await owner.create(pool=pool, idempotency_key=f"python-streaming-{pool}-{time.time_ns()}")
    path = "/workspace/round-trip.bin"
    digest = expected_digest(ROUND_TRIP_BYTES)
    try:
        await sandbox.files.write_text(path, "old-destination")
        first_chunk = asyncio.Event()
        continue_upload = asyncio.Event()
        upload_integrity = Integrity()
        upload = asyncio.create_task(
            sandbox.files.write_stream(
                path,
                chunks(
                    ROUND_TRIP_BYTES,
                    upload_integrity,
                    first_chunk=first_chunk,
                    continue_after_first=continue_upload,
                ),
                size_bytes=ROUND_TRIP_BYTES,
                sha256=digest,
            )
        )
        await asyncio.wait_for(first_chunk.wait(), timeout=10)
        assert await sandbox.files.read_text(path) == "old-destination"
        continue_upload.set()
        await upload
        assert (upload_integrity.count, upload_integrity.hexdigest()) == (ROUND_TRIP_BYTES, digest)
        await verify_download(sandbox, path, ROUND_TRIP_BYTES, digest)

        await malformed_digest_upload(
            subject_token(required("SANDBOX_TEST_SUBJECT_ID")), sandbox, path
        )

        async with sandbox.files.read_stream(path) as download:
            iterator = download.__aiter__()
            assert await anext(iterator)
        await sandbox.files.write_stream(
            "/workspace/permit-released.bin",
            one_chunk(b"permit-released"),
            size_bytes=len(b"permit-released"),
            sha256=hashlib.sha256(b"permit-released").hexdigest(),
        )
        await verify_download(
            sandbox,
            "/workspace/permit-released.bin",
            len(b"permit-released"),
            hashlib.sha256(b"permit-released").hexdigest(),
        )

        await verify_subject_scoping(sandbox, other)
        await verify_symlink_rejection(sandbox)
        artifacts = await sandbox.run("find /workspace -name '.asp-*' -print", check=True)
        assert artifacts.stdout == "", artifacts.stdout

        slow_started = asyncio.Event()
        slow_integrity = Integrity()
        slow_upload = asyncio.create_task(
            sandbox.files.write_stream(
                "/workspace/aborted.bin",
                chunks(ROUND_TRIP_BYTES, slow_integrity, first_chunk=slow_started, delay=0.05),
                size_bytes=ROUND_TRIP_BYTES,
                sha256=digest,
            )
        )
        await asyncio.wait_for(slow_started.wait(), timeout=10)
        release = asyncio.create_task(sandbox.release())
        try:
            try:
                await slow_upload
            except SandboxAbortedError as error:
                assert error.code == "ABORTED"
            else:
                raise AssertionError("release during upload did not abort the transfer")
        finally:
            released = await release
        assert released.status == "released"
        return {"pool": pool, "bytes": ROUND_TRIP_BYTES, "sha256": digest, "chunkBytes": CHUNK_BYTES}
    finally:
        await sandbox.close()


async def main() -> None:
    owner_subject = required("SANDBOX_TEST_SUBJECT_ID")
    other_subject = required("SANDBOX_TEST_OTHER_SUBJECT_ID")
    async with (
        SandboxClient(
            base_url=required("SANDBOX_PLATFORM_URL"),
            credentials=lambda: subject_token(owner_subject),
            timeout=180,
        ) as owner,
        SandboxClient(
            base_url=required("SANDBOX_PLATFORM_URL"),
            credentials=lambda: subject_token(other_subject),
            timeout=180,
        ) as other,
    ):
        results = [await verify_pool(pool, owner, other) for pool in ("coding", "browser")]
    print(json.dumps({"streaming": results, "status": "passed"}, separators=(",", ":")))


def required(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise RuntimeError(f"{name} is required")
    return value


asyncio.run(main())
