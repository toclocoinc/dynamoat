#!/usr/bin/env node
// Thin wrapper so `npx dynamoat-bench` (or the installed bin) runs the
// TypeScript CLI through the pinned tsx loader, wherever it is invoked from.
import { spawnSync } from "node:child_process";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const require = createRequire(import.meta.url);
const here = dirname(fileURLToPath(import.meta.url));
const cli = join(here, "..", "src", "cli.ts");
// Resolve tsx from this package's own node_modules, not the caller's cwd.
const tsxLoader = require.resolve("tsx");

const result = spawnSync(
  process.execPath,
  ["--import", tsxLoader, cli, ...process.argv.slice(2)],
  { stdio: "inherit" },
);
if (result.error) throw result.error;
process.exit(result.status ?? 1);
