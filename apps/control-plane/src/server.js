import { MAX_JSON_BODY_BYTES, SANDBOX_PATH } from "@geminixiang/sandbox-contracts";
import { createServer } from "node:http";

export function createControlPlaneServer(options) {
  const { backend, token } = options;
  return createServer(async (request, response) => {
    try {
      const url = new URL(request.url ?? "/", "http://control-plane.local");
      if (request.method === "GET" && url.pathname === "/health") {
        return sendJson(response, 200, { status: "ok" });
      }
      authorize(request, token);

      if (request.method === "POST" && url.pathname === `${SANDBOX_PATH}/acquire`) {
        const body = await readJson(request);
        requireString(body.key, "key");
        requireString(body.pool, "pool");
        return sendJson(response, 200, await backend.acquire(body));
      }

      const route = matchSandboxRoute(url.pathname);
      if (!route) return sendError(response, 404, "NOT_FOUND", "Route not found");
      const { id, action } = route;

      if (request.method === "GET" && !action) {
        return sendJson(response, 200, { sandbox: await backend.get(id) });
      }
      if (request.method === "DELETE" && !action) {
        await backend.delete(id);
        response.writeHead(204).end();
        return;
      }
      if (request.method === "POST" && action === "exec") {
        const body = await readJson(request);
        requireString(body.command, "command");
        return sendJson(response, 200, await backend.exec(id, body, request.signal));
      }
      if (request.method === "POST" && action === "files/read") {
        const body = await readJson(request);
        requireString(body.path, "path");
        requireEncoding(body.encoding);
        return sendJson(response, 200, await backend.readFile(id, body));
      }
      if (request.method === "POST" && action === "files/write") {
        const body = await readJson(request);
        requireString(body.path, "path");
        requireString(body.content, "content", true);
        requireEncoding(body.encoding);
        return sendJson(response, 200, await backend.writeFile(id, body));
      }
      if (request.method === "POST" && action === "release") {
        return sendJson(response, 200, { sandbox: await backend.release(id) });
      }
      return sendError(response, 405, "METHOD_NOT_ALLOWED", "Method not allowed");
    } catch (error) {
      const status = Number.isInteger(error.status) ? error.status : 500;
      const code = typeof error.code === "string" ? error.code : "INTERNAL_ERROR";
      const message = status === 500 ? "Internal server error" : error.message;
      sendError(response, status, code, message);
    }
  });
}

function matchSandboxRoute(pathname) {
  if (!pathname.startsWith(`${SANDBOX_PATH}/`)) return undefined;
  const parts = pathname.slice(SANDBOX_PATH.length + 1).split("/").map(decodeURIComponent);
  const id = parts.shift();
  if (!id) return undefined;
  return { id, action: parts.join("/") };
}

function authorize(request, token) {
  if (!token) return;
  if (request.headers.authorization !== `Bearer ${token}`) {
    throw httpError(401, "UNAUTHORIZED", "Invalid bearer token");
  }
}

async function readJson(request) {
  let size = 0;
  const chunks = [];
  for await (const chunk of request) {
    size += chunk.length;
    if (size > MAX_JSON_BODY_BYTES) throw httpError(413, "BODY_TOO_LARGE", "JSON body is too large");
    chunks.push(chunk);
  }
  try {
    return JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}");
  } catch {
    throw httpError(400, "INVALID_JSON", "Request body must be valid JSON");
  }
}

function requireString(value, field, allowEmpty = false) {
  if (typeof value !== "string" || (!allowEmpty && !value.trim())) {
    throw httpError(400, "INVALID_REQUEST", `'${field}' must be a non-empty string`);
  }
}

function requireEncoding(encoding) {
  if (encoding !== undefined && encoding !== "utf8" && encoding !== "base64") {
    throw httpError(400, "INVALID_REQUEST", "'encoding' must be 'utf8' or 'base64'");
  }
}

function sendJson(response, status, body) {
  response.writeHead(status, { "content-type": "application/json" });
  response.end(JSON.stringify(body));
}

function sendError(response, status, code, message) {
  if (response.headersSent) return response.end();
  sendJson(response, status, { error: { code, message } });
}

function httpError(status, code, message) {
  return Object.assign(new Error(message), { status, code });
}
