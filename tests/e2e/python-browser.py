from __future__ import annotations

import asyncio
import base64
import hashlib
import hmac
import json
import os
import time

from agent_sandbox import SandboxClient, StaticToken


def subject_token() -> str:
    consumer_id = required("SANDBOX_TEST_CONSUMER_ID")
    subject_id = required("SANDBOX_TEST_SUBJECT_ID")
    secret = required("SANDBOX_TEST_CONSUMER_SECRET")
    claims = json.dumps(
        {"consumerId": consumer_id, "subjectId": subject_id, "exp": int(time.time()) + 300},
        separators=(",", ":"),
    ).encode()
    payload = base64.urlsafe_b64encode(claims).decode().rstrip("=")
    signed = f"v1.{payload}"
    signature = base64.urlsafe_b64encode(
        hmac.new(secret.encode(), signed.encode(), hashlib.sha256).digest()
    ).decode().rstrip("=")
    return f"{signed}.{signature}"


async def main() -> None:
    source = """import { chromium } from "playwright-core";
const browser = await chromium.launch({ executablePath: "/usr/bin/chromium", headless: true });
try {
  const page = await browser.newPage();
  await page.goto("https://example.com", { waitUntil: "domcontentloaded" });
  await page.screenshot({ path: "/workspace/python-example.png", fullPage: true });
  console.log(JSON.stringify({ title: await page.title(), heading: await page.locator("h1").textContent(), url: page.url() }));
} finally { await browser.close(); }
"""
    async with SandboxClient(base_url=required("SANDBOX_PLATFORM_URL"), credentials=StaticToken(subject_token())) as client:
        async with client.sandbox(pool="browser", idempotency_key=f"python-browser-{time.time_ns()}") as sandbox:
            await sandbox.files.write_text("/workspace/python-browser.mjs", source)
            result = await sandbox.run(
                "test -e node_modules || ln -s /opt/browser/node_modules node_modules; node python-browser.mjs",
                cwd="/workspace",
                check=True,
            )
            payload = json.loads(result.stdout)
            assert payload == {
                "title": "Example Domain",
                "heading": "Example Domain",
                "url": "https://example.com/",
            }, payload
            screenshot = await sandbox.files.read_bytes("/workspace/python-example.png")
            assert len(screenshot) > 1000
            print(json.dumps({**payload, "screenshotBytes": len(screenshot)}))


def required(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise RuntimeError(f"{name} is required")
    return value


asyncio.run(main())
