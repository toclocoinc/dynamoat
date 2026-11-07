// Shared helpers for the smoke scripts. Each script is a sequence of checks
// driven through the AWS SDK v3 the way a desktop DynamoDB client would, so
// `node smoke/04-query-scan.mjs` runs one workflow against dynamoat or real
// DynamoDB and prints a readable result per check.

import { DynamoDBClient, DeleteTableCommand, DescribeTableCommand } from '@aws-sdk/client-dynamodb';
import { DynamoDBStreamsClient } from '@aws-sdk/client-dynamodb-streams';
import { DynamoDBDocumentClient } from '@aws-sdk/lib-dynamodb';
import { spawn, spawnSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { once } from 'node:events';
import { mkdtemp, rm } from 'node:fs/promises';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, '..');
const goScript = join(repoRoot, 'scripts', 'go.sh');
const serverDir = join(repoRoot, 'server');

// current holds the state of the script being run, so the check/table/send
// helpers stay small. Checks run one at a time, so module-level state is safe.
let current = null;
let lastRequest = null;

// Unsupported marks a check that is not applicable to the current target (for
// example GET /healthz only exists on dynamoat, not on real DynamoDB). Throwing
// it makes check() report "○ name: unsupported (<operation>)".
export class Unsupported extends Error {
  constructor(operation) {
    super(`unsupported: ${operation}`);
    this.name = 'UnsupportedSmoke';
    this.operation = operation;
  }
}

export function parseArgs(argv) {
  const args = { aws: false, start: false, strict: false, help: false };
  for (const a of argv) {
    if (a === '--aws') args.aws = true;
    else if (a === '--start') args.start = true;
    else if (a === '--strict') args.strict = true;
    else if (a === '--help' || a === '-h') args.help = true;
  }
  return args;
}

// startContext resolves the target: real DynamoDB (--aws), a locally built and
// started dynamoat (--start), or the ENDPOINT env var (default DynamoDB Local).
export async function startContext(args) {
  let server = null;
  let endpoint;
  if (args.aws) {
    endpoint = undefined;
  } else if (args.start) {
    server = await startServer();
    endpoint = server.endpoint;
  } else {
    endpoint = process.env.ENDPOINT ?? 'http://localhost:8000';
  }

  const region = process.env.AWS_REGION || process.env.AWS_DEFAULT_REGION || 'us-east-1';
  const credentials = endpoint ? { accessKeyId: 'dummy', secretAccessKey: 'dummy' } : undefined;
  const ddb = new DynamoDBClient({ region, ...(endpoint ? { endpoint } : {}), ...(credentials ? { credentials } : {}) });
  const doc = DynamoDBDocumentClient.from(ddb);
  const streams = new DynamoDBStreamsClient({
    region,
    ...(endpoint ? { endpoint } : {}),
    ...(credentials ? { credentials } : {}),
  });
  return { aws: args.aws, endpoint, region, ddb, doc, streams, server };
}

// startServer builds and starts a local dynamoat and waits for /readyz.
async function startServer() {
  const tmp = await mkdtemp(join(tmpdir(), 'dynamoat-smoke-'));
  const bin = join(tmp, 'dynamoat');
  const build = spawnSync('/bin/sh', [goScript, '-C', serverDir, 'build', '-o', bin, './cmd/dynamoat'], {
    cwd: here,
    stdio: 'inherit',
  });
  if (build.status !== 0) {
    throw new Error(`dynamoat build failed (exit ${build.status})`);
  }

  const port = await freePort();
  // Postgres when the caller points DYNAMOAT_PG at one, otherwise memory.
  const store = process.env.DYNAMOAT_PG ? 'postgres' : 'memory';
  const env = {
    ...process.env,
    DYNAMOAT_AUTH: 'none',
    DYNAMOAT_STORE: store,
    DYNAMOAT_LISTEN: `127.0.0.1:${port}`,
  };

  const log = [];
  const capture = (d) => {
    log.push(String(d));
    if (log.length > 400) log.shift();
  };
  const child = spawn(bin, [], { env, stdio: ['ignore', 'pipe', 'pipe'] });
  child.stdout.on('data', capture);
  child.stderr.on('data', capture);

  const endpoint = `http://127.0.0.1:${port}`;
  await waitReady(endpoint, child, log);

  return {
    endpoint,
    stop: async () => {
      if (child.exitCode === null) {
        child.kill('SIGTERM');
        await Promise.race([once(child, 'exit'), delay(5000)]);
      }
      await rm(tmp, { recursive: true, force: true }).catch(() => {});
    },
  };
}

// run is the entry point of every script: it resolves the target, runs the
// checks, deletes every table this script created (also on failure), stops the
// server it started, and prints one summary line.
export async function run(title, main) {
  const args = parseArgs(process.argv.slice(2));
  if (args.help) {
    process.stdout.write(
      `usage: node ${title}.mjs [--aws] [--start] [--strict]\n` +
        '  --aws     talk to real DynamoDB with the normal credential chain\n' +
        '  --start   build and start a local dynamoat on a free port\n' +
        '  --strict  fail the script when a check is unsupported\n',
    );
    return;
  }

  let ctx;
  try {
    ctx = await startContext(args);
  } catch (e) {
    process.stderr.write(`${title}: ${e.stack ?? e}\n`);
    process.exitCode = 1;
    return;
  }

  const results = [];
  const created = new Set();
  const prefix = randomBytes(6).toString('hex');
  current = { args, results, created, prefix };

  const started = Date.now();
  try {
    await main({ ...ctx, prefix, check, table, send, Unsupported, results });
  } catch (e) {
    results.push({ name: '(unexpected error)', status: 'fail', error: e });
    process.stderr.write(`${title}: unexpected: ${e.stack ?? e}\n`);
  } finally {
    await cleanupTables(ctx.ddb, created);
    if (ctx.server) await ctx.server.stop();
    current = null;
  }

  const passed = results.filter((r) => r.status === 'pass').length;
  const failed = results.filter((r) => r.status === 'fail').length;
  const unsupported = results.filter((r) => r.status === 'unsupported').length;
  const elapsed = Date.now() - started;

  process.stdout.write(
    `summary: ${results.length} checks, ${passed} passed, ${failed} failed, ${unsupported} unsupported (${elapsed} ms)\n`,
  );
  process.stdout.write(
    `SMOKE_SUMMARY title=${title} checks=${results.length} passed=${passed} failed=${failed} unsupported=${unsupported}\n`,
  );
  if (failed > 0 || (args.strict && unsupported > 0)) process.exitCode = 1;
}

// check runs one named assertion. A thrown Unsupported (or an
// UnknownOperationException / "dynamoat does not support ..." ValidationException
// from the endpoint) is reported as ○; any other error as ✖; success as ✔.
export async function check(name, fn) {
  const result = { name, status: 'pass', ms: 0 };
  const t0 = Date.now();
  try {
    await fn();
    result.ms = Date.now() - t0;
    process.stdout.write(`✔ ${name} (${result.ms} ms)\n`);
  } catch (e) {
    result.ms = Date.now() - t0;
    result.error = e;
    const operation = classifyUnsupported(e);
    if (operation) {
      result.status = 'unsupported';
      result.operation = operation;
      process.stdout.write(`○ ${name}: unsupported (${operation})\n`);
    } else {
      result.status = 'fail';
      process.stdout.write(`✖ ${name}: ${e?.name ?? 'Error'}: ${e?.message ?? e}\n`);
      if (lastRequest && isSDKError(e)) {
        process.stdout.write(`  request: ${lastRequest.op} ${JSON.stringify(lastRequest.input)}\n`);
      }
    }
  }
  current?.results.push(result);
}

// table returns a per-run table name, registered for deletion at the end.
export function table(name) {
  const t = `smoke-${current.prefix}-${name}`;
  current.created.add(t);
  return t;
}

// send runs one SDK command and remembers it, so check() can show the request
// that failed. It works with the low-level, document and streams clients.
export async function send(client, command) {
  lastRequest = { op: command.constructor.name.replace(/Command$/, ''), input: command.input };
  return client.send(command);
}

function classifyUnsupported(e) {
  if (e instanceof Unsupported) return e.operation;
  if (!e || typeof e !== 'object') return null;
  if (e.name === 'UnknownOperationException') return lastRequest?.op ?? 'operation';
  if (e.name === 'ValidationException') {
    const message = String(e.message ?? '');
    if (message.startsWith('dynamoat does not support') || message.startsWith('dynamoat:')) {
      return lastRequest?.op ?? 'operation';
    }
  }
  return null;
}

function isSDKError(e) {
  return e && typeof e === 'object' && '$metadata' in e;
}

async function cleanupTables(ddb, created) {
  for (const name of created) {
    try {
      await ddb.send(new DeleteTableCommand({ TableName: name }));
    } catch {
      // already gone, or the target does not allow deletion
    }
  }
}

export function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

// waitActive polls DescribeTable until the table and all its GSIs are ACTIVE.
export async function waitActive(ddb, tableName) {
  const deadline = Date.now() + 60000;
  while (Date.now() < deadline) {
    const res = await ddb.send(new DescribeTableCommand({ TableName: tableName }));
    const t = res.Table;
    const active = t.TableStatus === 'ACTIVE' && (t.GlobalSecondaryIndexes ?? []).every((g) => g.IndexStatus === 'ACTIVE');
    if (active) return t;
    await sleep(200);
  }
  throw new Error(`table ${tableName} did not become ACTIVE within 60s`);
}

// waitIndexActive polls until one GSI reaches ACTIVE (used after UpdateTable).
export async function waitIndexActive(ddb, tableName, indexName) {
  const deadline = Date.now() + 60000;
  while (Date.now() < deadline) {
    const res = await ddb.send(new DescribeTableCommand({ TableName: tableName }));
    const gsi = (res.Table.GlobalSecondaryIndexes ?? []).find((g) => g.IndexName === indexName);
    if (gsi?.IndexStatus === 'ACTIVE') return;
    await sleep(200);
  }
  throw new Error(`index ${indexName} of ${tableName} did not become ACTIVE within 60s`);
}

// waitGone polls until DescribeTable reports ResourceNotFoundException.
export async function waitGone(ddb, tableName) {
  const deadline = Date.now() + 60000;
  while (Date.now() < deadline) {
    try {
      await ddb.send(new DescribeTableCommand({ TableName: tableName }));
    } catch (e) {
      if (e?.name === 'ResourceNotFoundException') return;
      throw e;
    }
    await sleep(200);
  }
  throw new Error(`table ${tableName} did not disappear within 60s`);
}

function delay(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

async function freePort() {
  const srv = createServer();
  srv.listen(0, '127.0.0.1');
  await once(srv, 'listening');
  const port = srv.address().port;
  srv.close();
  await once(srv, 'close');
  return port;
}

async function waitReady(endpoint, child, log) {
  const deadline = Date.now() + 30000;
  let lastErr;
  while (Date.now() < deadline) {
    if (child.exitCode !== null) {
      throw new Error(`dynamoat exited with code ${child.exitCode}\n${log.join('')}`);
    }
    try {
      const res = await fetch(`${endpoint}/readyz`);
      if (res.ok) return;
      lastErr = new Error(`readyz status ${res.status}`);
    } catch (e) {
      lastErr = e;
    }
    await delay(150);
  }
  throw new Error(`dynamoat not ready: ${String(lastErr)}\n${log.join('')}`);
}
