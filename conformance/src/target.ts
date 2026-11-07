// Target resolution: build and run dynamoat locally, or talk to real DynamoDB.
// CONFORMANCE_TARGET=dynamoat (default) starts the server on a free port;
// CONFORMANCE_TARGET=aws uses the caller's credentials and AWS_REGION and
// prefixes every table name with a per-run random prefix. For the dynamoat
// target, CONFORMANCE_STORE=postgres selects the Postgres store (its
// connection string is taken from CONFORMANCE_PG) instead of memory.

import {
  DeleteBackupCommand,
  DeleteTableCommand,
  DescribeTableCommand,
  DynamoDBClient,
  UpdateTableCommand,
} from '@aws-sdk/client-dynamodb';
import { DynamoDBStreamsClient } from '@aws-sdk/client-dynamodb-streams';
import { DynamoDBDocumentClient } from '@aws-sdk/lib-dynamodb';
import { spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { once } from 'node:events';
import { mkdtemp, rm } from 'node:fs/promises';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, '..', '..');
const conformanceDir = join(here, '..');
const goScript = join(repoRoot, 'scripts', 'go.sh');
const serverDir = join(repoRoot, 'server');

export interface Target {
  kind: 'dynamoat' | 'aws';
  region: string;
  endpoint?: string;
  prefix: string;
  ddb: DynamoDBClient;
  doc: DynamoDBDocumentClient;
  streams: DynamoDBStreamsClient;
  tableName(base: string): string;
  trackBackup(arn: string): void;
  stop(): Promise<void>;
}

let targetPromise: Promise<Target> | null = null;

export function getTarget(): Promise<Target> {
  if (!targetPromise) targetPromise = startTarget();
  return targetPromise;
}

export async function stopTarget(): Promise<void> {
  if (targetPromise) {
    const t = await targetPromise;
    await t.stop();
  }
}

async function startTarget(): Promise<Target> {
  if (process.env.CONFORMANCE_TARGET === 'aws') return awsTarget();
  return dynamoatTarget();
}

function awsTarget(): Target {
  const region = process.env.AWS_REGION || process.env.AWS_DEFAULT_REGION || 'us-east-1';
  const prefix = `conformance-${randomBytes(6).toString('hex')}-`;
  const ddb = new DynamoDBClient({ region });
  const doc = DynamoDBDocumentClient.from(ddb);
  const streams = new DynamoDBStreamsClient({ region });
  const created = new Set<string>();
  const createdBackups = new Set<string>();
  return {
    kind: 'aws',
    region,
    prefix,
    ddb,
    doc,
    streams,
    tableName: (base) => {
      const name = prefix + base;
      created.add(name);
      return name;
    },
    trackBackup: (arn) => {
      createdBackups.add(arn);
    },
    stop: async () => {
      // Delete every table and backup this run created, even when a case
      // failed earlier. A failed table must not stop the rest being cleaned.
      await cleanupAwsResources(ddb, created, createdBackups);
      ddb.destroy();
      streams.destroy();
    },
  };
}

async function dynamoatTarget(): Promise<Target> {
  const tmp = await mkdtemp(join(tmpdir(), 'dynamoat-conformance-'));
  const bin = join(tmp, 'dynamoat');
  const log: string[] = [];
  const capture = (d: Buffer): void => {
    log.push(String(d));
    if (log.length > 400) log.shift();
  };

  let child: ReturnType<typeof spawn> | undefined;
  try {
    await build(bin, capture);
    const port = await freePort();
    const store = process.env.CONFORMANCE_STORE === 'postgres' ? 'postgres' : 'memory';
    const env = {
      ...process.env,
      DYNAMOAT_AUTH: 'none',
      DYNAMOAT_STORE: store,
      DYNAMOAT_LISTEN: `127.0.0.1:${port}`,
    };
    if (store === 'postgres' && process.env.CONFORMANCE_PG) {
      env.DYNAMOAT_PG = process.env.CONFORMANCE_PG;
    }
    child = spawn(bin, [], {
      env,
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    child.stdout.on('data', capture);
    child.stderr.on('data', capture);

    const endpoint = `http://127.0.0.1:${port}`;
    await waitReady(endpoint, child, log);

    const ddb = new DynamoDBClient({
      region: 'us-east-1',
      endpoint,
      credentials: { accessKeyId: 'dummy', secretAccessKey: 'dummy' },
    });
    const doc = DynamoDBDocumentClient.from(ddb);
    const streams = new DynamoDBStreamsClient({
      region: 'us-east-1',
      endpoint,
      credentials: { accessKeyId: 'dummy', secretAccessKey: 'dummy' },
    });

    return {
      kind: 'dynamoat',
      region: 'us-east-1',
      endpoint,
      prefix: '',
      ddb,
      doc,
      streams,
      tableName: (base) => base,
      trackBackup: () => {},
      stop: async () => {
        if (child && child.exitCode === null) {
          child.kill('SIGTERM');
          await Promise.race([once(child, 'exit'), delay(5000)]);
        }
        ddb.destroy();
        streams.destroy();
        await rm(tmp, { recursive: true, force: true });
      },
    };
  } catch (err) {
    if (child && child.exitCode === null) child.kill('SIGKILL');
    await rm(tmp, { recursive: true, force: true }).catch(() => {});
    throw new Error(`dynamoat target failed to start:\n${log.join('')}\n${String(err)}`);
  }
}

function build(bin: string, capture: (d: Buffer) => void): Promise<void> {
  return new Promise((resolve, reject) => {
    const child = spawn('/bin/sh', [goScript, '-C', serverDir, 'build', '-o', bin, './cmd/dynamoat'], {
      cwd: conformanceDir,
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    child.stdout.on('data', capture);
    child.stderr.on('data', capture);
    child.on('error', reject);
    child.on('exit', (code) => {
      if (code === 0) resolve();
      else reject(new Error(`go.sh build exited with code ${code}`));
    });
  });
}

async function freePort(): Promise<number> {
  const srv = createServer();
  srv.listen(0, '127.0.0.1');
  await once(srv, 'listening');
  const port = (srv.address() as { port: number }).port;
  srv.close();
  await once(srv, 'close');
  return port;
}

async function waitReady(endpoint: string, child: { exitCode: number | null }, log: string[]): Promise<void> {
  const deadline = Date.now() + 30000;
  let lastErr: unknown;
  while (Date.now() < deadline) {
    if (child.exitCode !== null) {
      throw new Error(`dynamoat exited with code ${child.exitCode}`);
    }
    try {
      const res = await fetch(`${endpoint}/readyz`);
      if (res.ok) return;
      lastErr = new Error(`readyz status ${res.status}`);
    } catch (err) {
      lastErr = err;
    }
    await delay(150);
  }
  throw new Error(`dynamoat not ready: ${String(lastErr)}\n${log.join('')}`);
}

function delay(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

// The cleanup below runs against the AWS target only. It is exported as
// standalone functions so it can be unit-tested with a fake client.

// DdbLike is the slice of DynamoDBClient the cleanup needs, so a test can pass
// a fake.
export interface DdbLike {
  send(command: unknown): Promise<unknown>;
}

export function isResourceNotFound(err: unknown): boolean {
  return (
    typeof err === 'object' &&
    err !== null &&
    (err as { name?: unknown }).name === 'ResourceNotFoundException'
  );
}

// cleanupAwsResources deletes every table and backup a run created. A failure
// on one resource is logged to stderr and does not stop the rest being
// cleaned, except that ResourceNotFoundException is ignored as already gone.
export async function cleanupAwsResources(
  ddb: DdbLike,
  tables: Iterable<string>,
  backupArns: Iterable<string>,
): Promise<void> {
  for (const name of tables) {
    await deleteAwsTable(ddb, name);
  }
  for (const arn of backupArns) {
    await deleteAwsBackup(ddb, arn);
  }
}

// How long cleanup waits for a table that is still being created, and how
// often it looks. Tests shorten them.
let waitCapMs = 30 * 60 * 1000;
let waitPollMs = 15000;

export function setCleanupWaitForTest(capMs: number, pollMs: number): void {
  waitCapMs = capMs;
  waitPollMs = pollMs;
}

// deleteAwsTable removes a table's replicas first (a table with a replica
// cannot be deleted), then deletes the table itself.
async function deleteAwsTable(ddb: DdbLike, name: string): Promise<void> {
  let replicas: { RegionName?: string; RegionStatus?: string }[] = [];
  try {
    type Desc = { Table?: { TableStatus?: string; Replicas?: { RegionName?: string; RegionStatus?: string }[] } };
    let desc = (await ddb.send(new DescribeTableCommand({ TableName: name }))) as Desc | null | undefined;
    // A table still being created (a restore in progress) cannot be deleted,
    // and its source backup cannot be deleted until it is done: wait for it.
    const deadline = Date.now() + waitCapMs;
    while ((desc?.Table?.TableStatus === 'CREATING' || desc?.Table?.TableStatus === 'UPDATING') && Date.now() < deadline) {
      await delay(waitPollMs);
      desc = (await ddb.send(new DescribeTableCommand({ TableName: name }))) as Desc | null | undefined;
    }
    replicas = desc?.Table?.Replicas ?? [];
  } catch (err) {
    if (isResourceNotFound(err)) return; // already gone
    console.error(`dynamoat conformance cleanup: describe table ${name} failed: ${String(err)}`);
  }

  for (const replica of replicas) {
    if (!replica.RegionName || replica.RegionStatus === 'DELETING') continue;
    try {
      await ddb.send(
        new UpdateTableCommand({
          TableName: name,
          ReplicaUpdates: [{ Delete: { RegionName: replica.RegionName } }],
        }),
      );
    } catch (err) {
      if (!isResourceNotFound(err)) {
        console.error(`dynamoat conformance cleanup: remove replica ${replica.RegionName} from ${name} failed: ${String(err)}`);
      }
    }
  }

  try {
    await ddb.send(new DeleteTableCommand({ TableName: name }));
  } catch (err) {
    if (isResourceNotFound(err)) return;
    console.error(`dynamoat conformance cleanup: delete table ${name} failed: ${String(err)}`);
  }
}

async function deleteAwsBackup(ddb: DdbLike, arn: string): Promise<void> {
  try {
    await ddb.send(new DeleteBackupCommand({ BackupArn: arn }));
  } catch (err) {
    if (isResourceNotFound(err) || (err as { name?: unknown })?.name === 'BackupNotFoundException') return;
    console.error(`dynamoat conformance cleanup: delete backup ${arn} failed: ${String(err)}`);
  }
}
