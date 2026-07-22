import { LEASE_PATH, MAX_JSON_BODY_BYTES } from "@geminixiang/sandbox-contracts";
import { createServer } from "node:http";
import { verifySubjectToken } from "./auth.js";

export function createControlPlaneServer(options) {
  const { backend, resolveConsumerSecret } = options;
  if (typeof resolveConsumerSecret !== "function") {
    throw new TypeError("resolveConsumerSecret is required");
  }

  return createServer(async (request, response) => {
    try {
      const url = new URL(request.url ?? "/", "http://control-plane.local");
      if (request.method === "GET" && url.pathname === "/health") {
        return sendJson(response, 200, { status: "ok" });
      }
      const scope = authenticate(request, resolveConsumerSecret);

      if (request.method === "POST" && url.pathname === LEASE_PATH) {
        const body = await readJson(request);
        requireString(body.pool, "pool");
        const idempotencyKey = requireHeader(request, "idempotency-key");
        return sendJson(response, 201, await backend.acquire(scope, { ...body, idempotencyKey }));
      }

      const route = matchLeaseRoute(url.pathname);
      if (!route) return sendError(response, 404, "NOT_FOUND", "Route not found");
      const { id, action } = route;

      if (request.method === "GET" && !action) {
        return sendJson(response, 200, { lease: await backend.get(scope, id) });
      }
      if (request.method === "DELETE" && !action) {
        await backend.delete(scope, id);
        response.writeHead(204).end();
        return;
      }
      if (request.method === "POST" && action === "exec") {
        const body = await readJson(request);
        requireString(body.command, "command");
        return sendJson(response, 200, await backend.exec(scope, id, body, request.signal));
      }
      if (request.method === "POST" && action === "files/read") {
        const body = await readJson(request);
        requireString(body.path, "path");
        requireEncoding(body.encoding);
        return sendJson(response, 200, await backend.readFile(scope, id, body));
      }
      if (request.method === "POST" && action === "files/write") {
        const body = await readJson(request);
        requireString(body.path, "path");
        requireString(body.content, "content", true);
        requireEncoding(body.encoding);
        return sendJson(response, 200, await backend.writeFile(scope, id, body));
      }
      if (request.method === "POST" && action === "release") {
        return sendJson(response, 200, { lease: await backend.release(scope, id) });
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

function matchLeaseRoute(pathname) {
  if (!pathname.startsWith(`${LEASE_PATH}/`)) return undefined;
  const parts = pathname.slice(LEASE_PATH.length + 1).split("/").map(decodeURIComponent);
  const id = parts.shift();
  if (!id) return undefined;
  return { id, action: parts.join("/") };
}

function authenticate(request, resolveConsumerSecret) {
  const authorization = request.headers.authorization;
  if (!authorization?.startsWith("Bearer ")) {
    throw httpError(401, "UNAUTHORIZED", "Invalid or expired subject token");
  }
  return verifySubjectToken(authorization.slice("Bearer ".length), resolveConsumerSecret);
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

function requireHeader(request, name) {
  const value = request.headers[name];
  if (typeof value !== "string" || !value.trim() || value.length > 200) {
    throw httpError(400, "INVALID_REQUEST", `'${name}' header is required`);
  }
  return value;
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
