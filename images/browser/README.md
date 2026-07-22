# Browser runtime image

Pinned Chromium and `playwright-core` runtime for the browser Golden Path. The image runs as a non-root user and keeps Chromium's own sandbox enabled; it does not pass `--no-sandbox`.

Build and load it into the dedicated Colima profile:

```bash
./scripts/local/build-browser.sh
```

Run the real gVisor browser E2E:

```bash
./scripts/local/browser-smoke.sh
```
