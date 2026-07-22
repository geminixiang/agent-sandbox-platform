from __future__ import annotations

import asyncio
import base64
import hashlib
import hmac
import json
import os
import time
from datetime import timedelta

from agent_sandbox import SandboxAbortedError, SandboxClient, StaticToken


def subject_token() -> str:
    claims = json.dumps(
        {
            "consumerId": required("SANDBOX_TEST_CONSUMER_ID"),
            "subjectId": "failure-recovery",
            "exp": int(time.time()) + 300,
        },
        separators=(",", ":"),
    ).encode()
    payload = base64.urlsafe_b64encode(claims).decode().rstrip("=")
    signed = f"v1.{payload}"
    signature = base64.urlsafe_b64encode(
        hmac.new(
            required("SANDBOX_TEST_CONSUMER_SECRET").encode(),
            signed.encode(),
            hashlib.sha256,
        ).digest()
    ).decode().rstrip("=")
    return f"{signed}.{signature}"


async def main() -> None:
    async with SandboxClient(
        base_url=required("SANDBOX_PLATFORM_URL"),
        credentials=StaticToken(subject_token()),
        timeout=timedelta(seconds=15).total_seconds(),
    ) as client:
        async with client.sandbox(
            pool="coding", idempotency_key=f"gke-failure-{time.time_ns()}"
        ) as sandbox:
            started = time.monotonic()
            try:
                await sandbox.run("sleep 30", timeout=timedelta(seconds=1))
            except SandboxAbortedError as error:
                elapsed = time.monotonic() - started
                assert error.code == "ABORTED" and error.status == 408, (
                    error.code,
                    error.status,
                )
                assert elapsed < 10, elapsed
            else:
                raise AssertionError("timed command unexpectedly succeeded")

            recovered = await sandbox.run("printf recovered", check=True)
            assert recovered.stdout == "recovered"
            print(
                json.dumps(
                    {
                        "commandTimeout": True,
                        "typedAbort": True,
                        "sandboxReusableAfterTimeout": True,
                    }
                )
            )


def required(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise RuntimeError(f"{name} is required")
    return value


asyncio.run(main())
