FROM node:22.19.0-bookworm-slim

ARG PLAYWRIGHT_VERSION=1.55.0

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates \
      chromium \
      fonts-liberation \
      fonts-noto-color-emoji \
      tini \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /opt/browser /workspace \
    && npm install --prefix /opt/browser --omit=dev "playwright-core@${PLAYWRIGHT_VERSION}" \
    && npm cache clean --force \
    && useradd --create-home --uid 10001 --shell /bin/bash sandbox \
    && chown -R sandbox:sandbox /workspace /opt/browser

ENV HOME=/home/sandbox \
    PLAYWRIGHT_BROWSERS_PATH=0 \
    CHROMIUM_EXECUTABLE_PATH=/usr/bin/chromium

COPY smoke.mjs /opt/browser/smoke.mjs

USER 10001:10001
WORKDIR /workspace
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["sh", "-c", "trap 'exit 0' TERM; while true; do sleep 3600; done"]
