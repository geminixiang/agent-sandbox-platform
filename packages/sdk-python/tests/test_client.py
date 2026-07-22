from __future__ import annotations

import base64
import json
from typing import Any

import httpx
import pytest

from agent_sandbox import SandboxClient, SandboxNotFoundError, StaticToken

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


def response(body: dict[str, Any], status: int = 200) -> httpx.Response:
    return httpx.Response(status, json=body)
