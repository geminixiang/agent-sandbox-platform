import { execFileSync } from "node:child_process";
import {
  copyFileSync,
  mkdtempSync,
  mkdirSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const rootDir = fileURLToPath(new URL("..", import.meta.url));
const workspaceDir = join(rootDir, "packages", "sdk-typescript");
const fixtureDir = join(workspaceDir, "test", "types");
const tempDir = mkdtempSync(join(tmpdir(), "sandbox-sdk-package-"));
const installDir = join(tempDir, "consumer");
const npm = process.platform === "win32" ? "npm.cmd" : "npm";
const node = process.execPath;
const tsc = join(rootDir, "node_modules", ".bin", process.platform === "win32" ? "tsc.cmd" : "tsc");

const requiredExports = [
  "CommandFailedError",
  "CommandResult",
  "FileDownload",
  "Sandbox",
  "SandboxAbortedError",
  "SandboxClient",
  "SandboxCursorExpiredError",
  "SandboxError",
  "SandboxExpiredError",
  "SandboxFileNotFoundError",
  "SandboxFiles",
  "SandboxIntegrityError",
  "SandboxInvalidCursorError",
  "SandboxNotActiveError",
  "SandboxNotFoundError",
  "SandboxPage",
  "SandboxQuotaExceededError",
  "SandboxStreamingNotSupportedError",
  "SandboxTransferLimitError",
  "SandboxTransferTooLargeError",
  "SandboxUnknownPoolError",
  "StaticToken",
];
const legacyExports = [
  "LeaseHandle",
  "SandboxPlatformClient",
  "SandboxPlatformError",
  "SandboxPlatformIntegrityError",
  "createSubjectToken",
];
const allowedFiles = new Set([
  "package/package.json",
  "package/README.md",
  "package/LICENSE",
  "package/src/index.d.ts",
  "package/src/index.js",
]);

try {
  const packResult = JSON.parse(
    execFileSync(npm, ["pack", "--ignore-scripts", "--json", "--pack-destination", tempDir], {
      cwd: workspaceDir,
      encoding: "utf8",
    }),
  )[0];
  const tarball = join(tempDir, packResult.filename);
  for (const file of packResult.files) {
    const packagedPath = `package/${file.path}`;
    if (!allowedFiles.has(packagedPath)) throw new Error(`Unexpected packaged file: ${file.path}`);
  }
  for (const required of ["package/package.json", "package/src/index.js", "package/src/index.d.ts"]) {
    if (!packResult.files.some(({ path }) => `package/${path}` === required)) {
      throw new Error(`Required packaged file is missing: ${required.slice(8)}`);
    }
  }

  mkdirSync(installDir);
  writeFileSync(join(installDir, "package.json"), JSON.stringify({ private: true, type: "module" }));
  execFileSync(
    npm,
    ["install", "--ignore-scripts", "--no-audit", "--no-fund", "--package-lock=false", tarball],
    { cwd: installDir, stdio: "pipe" },
  );

  const packageDir = join(installDir, "node_modules", "@geminixiang", "sandbox-sdk");
  const packageJson = JSON.parse(readFileSync(join(packageDir, "package.json"), "utf8"));
  if (packageJson.name !== "@geminixiang/sandbox-sdk" || packageJson.version !== "0.2.0-rc.1") {
    throw new Error(`Unexpected SDK identity: ${packageJson.name}@${packageJson.version}`);
  }
  if (packageJson.type !== "module" || packageJson.engines?.node !== ">=22.19.0") {
    throw new Error("SDK ESM or Node engine metadata is invalid");
  }
  if (
    packageJson.exports?.["."]?.types !== "./src/index.d.ts" ||
    packageJson.exports?.["."]?.import !== "./src/index.js"
  ) {
    throw new Error("SDK root export metadata is invalid");
  }
  if (Object.keys(packageJson.dependencies ?? {}).length !== 0) {
    throw new Error("SDK must not have runtime dependencies");
  }

  const importCheck = `
    import * as sdk from "@geminixiang/sandbox-sdk";
    const expected = ${JSON.stringify([...requiredExports, ...legacyExports])};
    for (const name of expected) if (!(name in sdk)) throw new Error("Missing export: " + name);
    if (sdk.SandboxPlatformClient !== sdk.SandboxClient) throw new Error("Legacy client is not an alias");
    if (sdk.LeaseHandle !== sdk.Sandbox) throw new Error("Legacy lease is not an alias");
  `;
  execFileSync(node, ["--input-type=module", "--eval", importCheck], {
    cwd: installDir,
    stdio: "pipe",
  });

  copyFileSync(join(fixtureDir, "new-consumer.ts"), join(installDir, "new-consumer.ts"));
  copyFileSync(join(fixtureDir, "legacy-consumer.ts"), join(installDir, "legacy-consumer.ts"));
  copyFileSync(
    join(fixtureDir, "no-disposable-consumer.ts"),
    join(installDir, "no-disposable-consumer.ts"),
  );
  writeFileSync(
    join(installDir, "tsconfig.json"),
    JSON.stringify({
      compilerOptions: {
        target: "ES2022",
        module: "NodeNext",
        moduleResolution: "NodeNext",
        lib: ["ES2022", "DOM", "ESNext.Disposable"],
        strict: true,
        noEmit: true,
        skipLibCheck: false,
      },
      include: ["./*.ts"],
    }),
  );
  execFileSync(tsc, ["-p", "tsconfig.json"], { cwd: installDir, stdio: "pipe" });
  writeFileSync(
    join(installDir, "tsconfig.no-disposable.json"),
    JSON.stringify({
      compilerOptions: {
        target: "ES2022",
        module: "NodeNext",
        moduleResolution: "NodeNext",
        lib: ["ES2022", "DOM"],
        strict: true,
        noEmit: true,
        skipLibCheck: false,
      },
      files: ["./no-disposable-consumer.ts"],
    }),
  );
  execFileSync(tsc, ["-p", "tsconfig.no-disposable.json"], { cwd: installDir, stdio: "pipe" });

  const sizeKiB = Math.ceil(statSync(tarball).size / 1024);
  if (sizeKiB > 100) throw new Error(`SDK package is unexpectedly large: ${sizeKiB} KiB`);
  console.log(
    `Verified ${packResult.filename}: bare ESM import, new/legacy exports, installed types, ${sizeKiB} KiB, zero runtime dependencies.`,
  );
} finally {
  rmSync(tempDir, { recursive: true, force: true });
}
