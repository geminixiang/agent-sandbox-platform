import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const rootDir = fileURLToPath(new URL("..", import.meta.url));
const workspaceDir = join(rootDir, "packages", "sdk-typescript");
const tempDir = mkdtempSync(join(tmpdir(), "sandbox-sdk-package-"));
const npm = process.platform === "win32" ? "npm.cmd" : "npm";

try {
  const result = JSON.parse(
    execFileSync(npm, ["pack", "--ignore-scripts", "--json", "--pack-destination", tempDir], {
      cwd: workspaceDir,
      encoding: "utf8",
    }),
  )[0];
  const tarball = join(tempDir, result.filename);
  const installDir = join(tempDir, "install");
  execFileSync(npm, ["install", "--ignore-scripts", "--no-audit", "--no-fund", "--prefix", installDir, tarball]);

  const packageDir = join(installDir, "node_modules", "@geminixiang", "sandbox-sdk");
  const packageJson = JSON.parse(readFileSync(join(packageDir, "package.json"), "utf8"));
  if (Object.keys(packageJson.dependencies ?? {}).length !== 0) {
    throw new Error("SDK must not have runtime dependencies");
  }
  const sdk = await import(pathToFileURL(join(packageDir, "src", "index.js")).href);
  if (typeof sdk.SandboxPlatformClient !== "function") throw new Error("SDK export is missing");

  const sizeKiB = Math.ceil(statSync(tarball).size / 1024);
  if (sizeKiB > 100) throw new Error(`SDK package is unexpectedly large: ${sizeKiB} KiB`);
  console.log(`Verified ${result.filename}: ${sizeKiB} KiB, zero runtime dependencies.`);
} finally {
  rmSync(tempDir, { recursive: true, force: true });
}
