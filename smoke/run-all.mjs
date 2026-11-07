// run-all: run every smoke script in order, sharing one locally started
// dynamoat when --start is given, and print a table of results per script.

import { spawn } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseArgs, startContext } from './lib.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const scripts = [
  '01-connection.mjs',
  '02-table-admin.mjs',
  '03-items.mjs',
  '04-query-scan.mjs',
  '05-bulk.mjs',
  '06-export-import.mjs',
  '07-transactions.mjs',
  '08-streams.mjs',
  '09-partiql.mjs',
  '10-admin-extras.mjs',
];

const args = parseArgs(process.argv.slice(2));

let server;
let env = { ...process.env };
const childArgs = [];
if (args.strict) childArgs.push('--strict');
if (args.aws) {
  childArgs.push('--aws');
  delete env.ENDPOINT;
} else if (args.start) {
  const ctx = await startContext({ aws: false, start: true });
  server = ctx.server;
  env = { ...process.env, ENDPOINT: ctx.endpoint };
  process.stdout.write(`smoke: started dynamoat at ${ctx.endpoint}\n`);
}

const rows = [];
let failed = 0;
for (const script of scripts) {
  const row = await runOne(script, childArgs, env);
  rows.push(row);
  failed += row.failed;
}

if (server) await server.stop();

printTable(rows);
process.stdout.write(`\ntotal: ${rows.length} scripts, ${failed} failed\n`);
process.exitCode = failed > 0 ? 1 : 0;

function runOne(script, childArgs, env) {
  return new Promise((resolve) => {
    const child = spawn(process.execPath, [join(here, script), ...childArgs], {
      cwd: here,
      env,
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let out = '';
    let err = '';
    child.stdout.on('data', (d) => (out += d));
    child.stderr.on('data', (d) => (err += d));
    child.on('close', (code) => {
      process.stdout.write(`\n== ${script} ==\n${out}`);
      if (err) process.stderr.write(err);
      const m = out.match(/SMOKE_SUMMARY title=\S+ checks=(\d+) passed=(\d+) failed=(\d+) unsupported=(\d+)/);
      if (m) {
        resolve({ script, code, checks: Number(m[1]), passed: Number(m[2]), failed: Number(m[3]), unsupported: Number(m[4]) });
      } else {
        resolve({ script, code, checks: 0, passed: 0, failed: 1, unsupported: 0 });
      }
    });
  });
}

function printTable(rows) {
  const header = ['script', 'checks', 'passed', 'failed', 'unsupported'];
  const cells = rows.map((r) => [r.script, String(r.checks), String(r.passed), String(r.failed), String(r.unsupported)]);
  const widths = header.map((h, c) => Math.max(h.length, ...cells.map((row) => row[c].length)));
  const line = (arr) => arr.map((cell, c) => cell.padEnd(widths[c])).join('  ').trimEnd();
  process.stdout.write(`\n${line(header)}\n`);
  for (const row of cells) process.stdout.write(`${line(row)}\n`);
}
