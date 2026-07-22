from __future__ import annotations

import asyncio
import base64
import hashlib
import hmac
import json
import os
import time
from datetime import timedelta

from agent_sandbox import SandboxClient, SandboxNotFoundError, StaticToken


def token(subject: str) -> str:
    claims = json.dumps(
        {"consumerId": required("SANDBOX_TEST_CONSUMER_ID"), "subjectId": subject, "exp": int(time.time()) + 300},
        separators=(",", ":"),
    ).encode()
    payload = base64.urlsafe_b64encode(claims).decode().rstrip("=")
    signed = f"v1.{payload}"
    signature = base64.urlsafe_b64encode(
        hmac.new(required("SANDBOX_TEST_CONSUMER_SECRET").encode(), signed.encode(), hashlib.sha256).digest()
    ).decode().rstrip("=")
    return f"{signed}.{signature}"


async def coding_workflow(client: SandboxClient, fixture: str) -> tuple[str, dict[str, object]]:
    started = time.monotonic()
    sandbox = await client.create(pool="coding", idempotency_key=f"gke-coding-{time.time_ns()}")
    acquired_ms = round((time.monotonic() - started) * 1000)
    crawler = '''import gzip, json, sys
from html.parser import HTMLParser
from urllib.parse import urljoin
from urllib.request import Request, urlopen
from urllib.robotparser import RobotFileParser

base = sys.argv[1]
robots = RobotFileParser(urljoin(base, "/robots.txt")); robots.read()
assert robots.can_fetch("work-e2e", urljoin(base, "/catalog/1"))
assert not robots.can_fetch("work-e2e", urljoin(base, "/private"))

class Links(HTMLParser):
    def __init__(self): super().__init__(); self.links=[]
    def handle_starttag(self, tag, attrs):
        if tag == "a":
            values=dict(attrs)
            if "href" in values: self.links.append((values.get("class"), values.get("rel"), values["href"]))

def get(url):
    with urlopen(Request(url, headers={"User-Agent": "work-e2e/1.0"}), timeout=10) as response:
        return response.geturl(), response.read(), dict(response.headers)

redirected, page, _ = get(urljoin(base, "/redirect"))
assert redirected.endswith("/catalog/1")
queue=[redirected]; visited=set(); items=[]
while queue:
    url=queue.pop(0)
    if url in visited: continue
    visited.add(url)
    _, body, _=get(url)
    parser=Links(); parser.feed(body.decode())
    for css, rel, href in parser.links:
        absolute=urljoin(url, href)
        if css == "item": items.append(absolute)
        if rel == "next": queue.append(absolute)
_, compressed, headers=get(urljoin(base, "/gzip"))
# urllib transparently leaves gzip encoded, unlike curl/browser.
if headers.get("Content-Encoding") == "gzip": compressed=gzip.decompress(compressed)
assert json.loads(compressed)["compressed"] is True
print(json.dumps({"items": sorted(items), "pages": len(visited), "robotsRespected": True}))
'''
    try:
        await sandbox.files.write_text("/workspace/crawler.py", crawler)
        result = await sandbox.run(f"python crawler.py {fixture}", cwd="/workspace")
        if result.exit_code != 0:
            raise AssertionError(f"crawler failed ({result.exit_code}): {result.stderr or result.stdout}")
        crawl = json.loads(result.stdout)
        assert crawl["pages"] == 2 and len(crawl["items"]) == 3 and crawl["robotsRespected"] is True

        await sandbox.files.write_text("/workspace/notes.txt", "persistent across exec calls\n")
        persisted = await sandbox.run("cat notes.txt && id -u", cwd="/workspace", check=True)
        lines = persisted.stdout.strip().splitlines()
        assert lines == ["persistent across exec calls", "10001"]

        github = await sandbox.run(
            "set -eu; curl -fsSL --max-time 15 -H 'User-Agent: agent-sandbox-e2e' https://api.github.com/repos/kubernetes-sigs/agent-sandbox | jq -r '.full_name'; git clone --depth 1 --filter=blob:none https://github.com/octocat/Hello-World.git external-repo >/dev/null 2>&1; test -f external-repo/README",
            cwd="/workspace",
            check=True,
            timeout=timedelta(seconds=45),
        )
        assert github.stdout.strip() == "kubernetes-sigs/agent-sandbox"
        return sandbox.id, {"acquireMs": acquired_ms, "crawl": crawl, "githubHttps": True, "gitClone": True}
    except BaseException:
        await sandbox.close()
        raise


async def browser_workflow(client: SandboxClient, fixture: str) -> tuple[str, dict[str, object]]:
    started = time.monotonic()
    sandbox = await client.create(pool="browser", idempotency_key=f"gke-browser-{time.time_ns()}")
    acquired_ms = round((time.monotonic() - started) * 1000)
    script = '''import { chromium } from "playwright-core";
import fs from "node:fs";
const [fixture] = process.argv.slice(2);
const browser = await chromium.launch({ executablePath: "/usr/bin/chromium", headless: true });
try {
  const page = await browser.newPage({ acceptDownloads: true });
  await page.goto(fixture + "/app", { waitUntil: "domcontentloaded" });
  await page.locator("#status").waitFor({ state: "visible" });
  await page.waitForFunction(() => document.querySelector("#status")?.textContent === "Ready");
  await page.getByLabel("Search").fill("release notes");
  await page.getByRole("button", { name: "Search" }).click();
  await page.waitForFunction(() => document.querySelector("#result")?.textContent.includes("release notes"));
  const popupPromise = page.waitForEvent("popup");
  await page.locator("#popup").click();
  const popup = await popupPromise; await popup.waitForLoadState();
  const downloadPromise = page.waitForEvent("download");
  await page.locator("#download").click();
  const download = await downloadPromise; await download.saveAs("/workspace/report.csv");
  await page.screenshot({ path: "/workspace/dashboard.png", fullPage: true });
  const external = await browser.newPage();
  await external.goto("https://example.com", { waitUntil: "domcontentloaded", timeout: 15000 });
  console.log(JSON.stringify({ title: await page.title(), status: await page.locator("#status").textContent(), result: JSON.parse(await page.locator("#result").textContent()), popup: await popup.title(), external: await external.title(), report: fs.readFileSync("/workspace/report.csv", "utf8") }));
} finally { await browser.close(); }
'''
    try:
        await sandbox.files.write_text("/workspace/workflow.mjs", script)
        result = await sandbox.run(
            f"test -e node_modules || ln -s /opt/browser/node_modules node_modules; node workflow.mjs {fixture}",
            cwd="/workspace",
            timeout=timedelta(seconds=45),
        )
        if result.exit_code != 0:
            raise AssertionError(f"browser workflow failed ({result.exit_code}): {result.stderr or result.stdout}")
        payload = json.loads(result.stdout)
        assert payload["title"] == "Work Dashboard" and payload["status"] == "Ready"
        assert payload["result"]["query"] == "release notes" and payload["popup"] == "Details"
        assert payload["external"] == "Example Domain" and "alpha,35" in payload["report"]
        screenshot = await sandbox.files.read_bytes("/workspace/dashboard.png")
        assert len(screenshot) > 1_000
        return sandbox.id, {"acquireMs": acquired_ms, "screenshotBytes": len(screenshot), "form": True, "popup": True, "download": True, "externalHttps": True}
    except BaseException:
        await sandbox.close()
        raise


async def main() -> None:
    base_url = required("SANDBOX_PLATFORM_URL")
    fixture = required("SANDBOX_FIXTURE_URL")
    owner = SandboxClient(base_url=base_url, credentials=StaticToken(token("daily-work-owner")), timeout=60)
    outsider = SandboxClient(base_url=base_url, credentials=StaticToken(token("daily-work-outsider")), timeout=30)
    coding = browser = None
    try:
        results = await asyncio.gather(
            coding_workflow(owner, fixture), browser_workflow(owner, fixture), return_exceptions=True
        )
        failures = [result for result in results if isinstance(result, Exception)]
        if failures:
            raise ExceptionGroup("daily-work workflows failed", failures)
        coding_result_pair, browser_result_pair = results
        if not isinstance(coding_result_pair, tuple) or not isinstance(browser_result_pair, tuple):
            raise AssertionError("workflow returned an invalid result")
        coding, coding_result = coding_result_pair
        browser, browser_result = browser_result_pair
        for lease_id in (coding, browser):
            try:
                await outsider.get(lease_id)
            except SandboxNotFoundError:
                pass
            else:
                raise AssertionError("cross-subject lease lookup was not isolated")
        print(json.dumps({"coding": coding_result, "browser": browser_result, "crossSubjectIsolation": True, "leaseIds": [coding, browser]}))
    finally:
        for lease_id in (coding, browser):
            if lease_id:
                try:
                    sandbox = await owner.get(lease_id)
                    await sandbox.release()
                except SandboxNotFoundError:
                    pass
        await outsider.close()
        await owner.close()


def required(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise RuntimeError(f"{name} is required")
    return value


asyncio.run(main())
