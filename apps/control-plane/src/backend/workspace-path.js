import { posix } from "node:path";

export function normalizeWorkspacePath(path) {
  if (typeof path !== "string" || !path.trim()) throw invalidPath();
  const absolute = path.startsWith("/") ? posix.normalize(path) : posix.resolve("/workspace", path);
  if (absolute !== "/workspace" && !absolute.startsWith("/workspace/")) throw invalidPath();
  return absolute;
}

function invalidPath() {
  return Object.assign(new Error("Path must stay inside /workspace"), {
    status: 400,
    code: "INVALID_PATH",
  });
}
