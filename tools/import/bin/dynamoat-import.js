#!/usr/bin/env node
// Entry point for the installed "dynamoat-import" command. It runs the
// TypeScript CLI with the tsx loader so the package is usable without a
// separate compile step. The loader path is resolved from this file, not the
// caller's working directory, so "npx dynamoat-import" works from anywhere.
import { spawnSync } from "node:child_process";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";

const require = createRequire(import.meta.url);
const tsxLoader = require.resolve("tsx");
const cli = fileURLToPath(new URL("../src/cli.ts", import.meta.url));

const result = spawnSync(
  process.execPath,
  ["--import", tsxLoader, cli, ...process.argv.slice(2)],
  { stdio: "inherit" },
);

process.exit(result.status ?? 1);
