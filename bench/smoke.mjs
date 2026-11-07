// Smoke test: build dynamoat, run every scenario against it for 3 s, and skip
// the scenarios whose operation the server does not implement yet (it answers
// UnknownOperationException). Run with `npm run smoke`.
import { spawn, spawnSync } from "node:child_process";
import { createRequire } from "node:module";
import { createServer } from "node:net";
import { mkdtempSync, openSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const benchDir = dirname(fileURLToPath(import.meta.url));
const require = createRequire(import.meta.url);
const tsxLoader = require.resolve("tsx");
const cli = join(benchDir, "src", "cli.ts");
const serverBin = join(benchDir, "..", "server", "dynamoat");
const go = join(benchDir, "..", "scripts", "go.sh");

function log(msg) {
  process.stdout.write(`smoke: ${msg}\n`);
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function freePort() {
  return new Promise((resolve, reject) => {
    const srv = createServer();
    srv.on("error", reject);
    srv.listen(0, "127.0.0.1", () => {
      const port = srv.address().port;
      srv.close(() => resolve(port));
    });
  });
}

async function waitReady(port, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(`http://127.0.0.1:${port}/readyz`);
      if (res.ok) return;
    } catch {
      // not up yet
    }
    await sleep(100);
  }
  throw new Error("server did not become ready");
}

function fmt(n) {
  return Number.isFinite(n) ? n.toFixed(3) : "-";
}

function printTable(reports) {
  const header = ["scenario", "requests", "throughput/s", "errors", "p50 ms", "p90 ms", "p99 ms", "p99.9 ms", "max ms"];
  const rows = reports.map((r) => [
    r.name,
    String(r.requests),
    fmt(r.throughput),
    Object.entries(r.errors).map(([k, v]) => `${k}=${v}`).join(", ") || "-",
    fmt(r.latencyMs.p50),
    fmt(r.latencyMs.p90),
    fmt(r.latencyMs.p99),
    fmt(r.latencyMs.p99_9),
    fmt(r.latencyMs.max),
  ]);
  const widths = header.map((_, c) => Math.max(header[c].length, ...rows.map((row) => row[c].length)));
  const line = (cells) => cells.map((cell, c) => cell.padEnd(widths[c])).join("  ").trimEnd();
  process.stdout.write(line(header) + "\n");
  for (const row of rows) process.stdout.write(line(row) + "\n");
}

async function main() {
  const tmp = mkdtempSync(join(tmpdir(), "dynamoat-bench-smoke-"));

  log(`building dynamoat (${go})`);
  const build = spawnSync("sh", [go, "-C", "../server", "build", "./cmd/dynamoat"], {
    cwd: benchDir,
    stdio: "inherit",
  });
  if (build.status !== 0) {
    process.stderr.write("smoke: build failed\n");
    process.exit(1);
  }

  const port = await freePort();
  const serverLog = join(tmp, "server.log");
  const server = spawn(serverBin, [], {
    cwd: benchDir,
    env: {
      ...process.env,
      DYNAMOAT_LISTEN: `127.0.0.1:${port}`,
      DYNAMOAT_AUTH: "none",
      DYNAMOAT_STORE: "memory",
      DYNAMOAT_LOG: "text",
    },
    stdio: ["ignore", openSync(serverLog, "w"), openSync(serverLog, "w")],
  });

  try {
    await waitReady(port, 30_000);

    const outJson = join(tmp, "smoke.json");
    log(`running every scenario for 3 s with concurrency 4 against 127.0.0.1:${port}`);
    const run = spawnSync(
      process.execPath,
      [
        "--import", tsxLoader,
        cli,
        "--endpoint", `http://127.0.0.1:${port}`,
        "--region", "us-east-1",
        "--scenario", "all",
        "--concurrency", "4",
        "--duration", "3s",
        "--warmup", "1s",
        "--seed-items", "400",
        "--out", outJson,
      ],
      { cwd: benchDir, encoding: "utf8" },
    );
    if (run.status !== 0) {
      process.stderr.write(run.stderr || "");
      process.stderr.write("smoke: benchmark run failed\n");
      process.exit(1);
    }

    const data = JSON.parse(readFileSync(outJson, "utf8"));
    const skipped = [];
    const ran = [];
    for (const r of data.scenarios) {
      const unknown = r.errors["UnknownOperationException"] ?? 0;
      if (r.requests > 0 && unknown === r.requests) {
        skipped.push(r.name);
      } else {
        ran.push(r);
      }
    }
    for (const name of skipped) {
      log(`skip: ${name} (the server answers UnknownOperationException)`);
    }
    printTable(ran);
  } finally {
    server.kill("SIGTERM");
    await Promise.race([
      new Promise((resolve) => server.on("exit", resolve)),
      sleep(2000),
    ]);
    try {
      rmSync(tmp, { recursive: true, force: true });
    } catch {
      // best effort
    }
  }
}

main().catch((e) => {
  process.stderr.write(`smoke: ${e.stack ?? e}\n`);
  process.exit(1);
});
