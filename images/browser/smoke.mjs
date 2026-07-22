import { chromium } from "playwright-core";

const browser = await chromium.launch({
  executablePath: process.env.CHROMIUM_EXECUTABLE_PATH ?? "/usr/bin/chromium",
  headless: true,
});
try {
  const page = await browser.newPage();
  await page.setContent(`<!doctype html><html><body><button id="run">ready</button><output></output><script>document.querySelector('#run').onclick=()=>document.querySelector('output').textContent='clicked'</script></body></html>`);
  await page.locator("#run").click();
  const output = await page.locator("output").textContent();
  if (output !== "clicked") throw new Error(`unexpected browser output ${output}`);
  await page.screenshot({ path: "/workspace/browser-smoke.png" });
  console.log(JSON.stringify({ chromium: await browser.version(), output }));
} finally {
  await browser.close();
}
