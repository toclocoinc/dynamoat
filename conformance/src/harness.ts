// expectSame runs one test case: a sequence of SDK calls, each captured as its
// result or error and normalised. With CONFORMANCE_RECORD=1 (aws target only)
// it writes the normalised capture to conformance/golden/<name>.json; otherwise
// it compares against that golden file, or reports "no golden yet".

import { CreateTableCommand, DescribeTableCommand } from '@aws-sdk/client-dynamodb';
import { goldenExists, readGolden, writeGolden, diff } from './golden.ts';
import { cleanError, cleanResult, makeNormaliser, sortItems } from './normalise.ts';
import type { Capture } from './normalise.ts';
import { getTarget } from './target.ts';
import type { Target } from './target.ts';

export interface Ctx {
  target: Target;
  ddb: Target['ddb'];
  doc: Target['doc'];
  streams: Target['streams'];
  table(base: string): string;
  call(promise: Promise<unknown> | (() => Promise<unknown>)): Promise<unknown>;
  // capture records one synthetic result (already normalised later) without
  // making an SDK call; label names it in the golden file.
  capture(label: string, value: unknown): unknown;
  createTable(input: Record<string, unknown>): Promise<unknown>;
}

export type CaseFn = (ctx: Ctx) => Promise<void>;

// CaseSpec is the shape every suite case provides. slow marks a case whose AWS
// polling can take minutes (skipped with CONFORMANCE_SKIP_SLOW=1);
// dynamoatOnly marks a case that only behaves as DynamoDB does on dynamoat.
export interface CaseSpec {
  name: string;
  requires: string[];
  slow?: boolean;
  dynamoatOnly?: boolean;
  fn: CaseFn;
}

export async function expectSame(
  spec: CaseSpec,
  t: { skip(message?: string): void },
): Promise<void> {
  const target = await getTarget();

  if (spec.dynamoatOnly && target.kind !== 'dynamoat') {
    t.skip(`skipped: ${spec.name} is dynamoat-only (real DynamoDB reports replicas CREATING for minutes)`);
    return;
  }

  if (spec.slow && process.env.CONFORMANCE_SKIP_SLOW === '1') {
    t.skip(`skipped: ${spec.name} is slow (unset CONFORMANCE_SKIP_SLOW to run it)`);
    return;
  }

  const missing = await missingOperations(target, spec.requires);
  if (missing.length > 0) {
    t.skip(`skipped: dynamoat does not implement ${missing.join(', ')} (UnknownOperationException)`);
    return;
  }

  const captures: Capture[] = [];
  const ctx: Ctx = {
    target,
    ddb: target.ddb,
    doc: target.doc,
    streams: target.streams,
    table: (base) => target.tableName(base),
    call: async (promise) => {
      const p = typeof promise === 'function' ? promise() : promise;
      try {
        const result = await p;
        const clean = cleanResult(result);
        captures.push({ result: clean });
        return clean;
      } catch (err) {
        const clean = cleanError(err);
        captures.push({ error: clean });
        return clean;
      }
    },
    capture: (label, value) => {
      const clean = cleanResult(value);
      captures.push({ label, result: clean });
      return clean;
    },
    createTable: async (input) => {
      const tableName = (input as { TableName?: string }).TableName;
      const clean = await ctx.call(() => target.ddb.send(new CreateTableCommand(input as never)));
      await waitActive(target, tableName ?? '');
      return clean;
    },
  };

  await spec.fn(ctx);

  const normalise = makeNormaliser({ prefix: target.prefix });
  const normalised = normalise(captures);

  if (process.env.CONFORMANCE_RECORD === '1') {
    if (target.kind !== 'aws') {
      throw new Error('CONFORMANCE_RECORD=1 requires CONFORMANCE_TARGET=aws');
    }
    await writeGolden(spec.name, normalised);
    return;
  }

  if (goldenExists(spec.name)) {
    const expected = await readGolden(spec.name);
    const diffs = diff(expected, normalised);
    if (diffs.length > 0) {
      throw new Error(`golden mismatch for ${spec.name}:\n${diffs.join('\n')}`);
    }
    return;
  }

  console.log(`no golden yet: ${spec.name}`);
}

// missingOperations returns the required operations dynamoat does not register.
// An operation is probed with a raw POST once; UnknownOperationException means
// it is missing, anything else means it is present.
async function missingOperations(target: Target, ops: string[]): Promise<string[]> {
  if (target.kind !== 'dynamoat' || ops.length === 0) return [];
  const missing: string[] = [];
  for (const op of ops) {
    if (!(await probeOperation(target, op))) missing.push(op);
  }
  return missing;
}

async function probeOperation(target: Target, op: string): Promise<boolean> {
  const res = await fetch(target.endpoint as string, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/x-amz-json-1.0',
      'X-Amz-Target': `DynamoDB_20120810.${op}`,
    },
    body: '{}',
  });
  const body = (await res.json().catch(() => ({}))) as { __type?: unknown };
  const type = body?.__type;
  return !(typeof type === 'string' && type.endsWith('#UnknownOperationException'));
}

// waitActive polls DescribeTable until the table and all its GSIs are ACTIVE.
// dynamoat reports ACTIVE immediately; real DynamoDB needs a moment.
async function waitActive(target: Target, tableName: string): Promise<void> {
  const deadline = Date.now() + 30000;
  while (Date.now() < deadline) {
    const res = (await target.ddb.send(new DescribeTableCommand({ TableName: tableName }))) as {
      Table?: { TableStatus?: string; GlobalSecondaryIndexes?: { IndexStatus?: string }[] };
    };
    const desc = res.Table;
    const active =
      desc?.TableStatus === 'ACTIVE' &&
      (desc.GlobalSecondaryIndexes ?? []).every((g) => g.IndexStatus === 'ACTIVE');
    if (active) return;
    await delay(150);
  }
  throw new Error(`table ${tableName} did not become ACTIVE within 30s`);
}

// captureScan pages a read whose order DynamoDB does not define (a Scan or a
// full-table PartiQL SELECT) with plain sends, so the per-page items and
// LastEvaluatedKey/NextToken never reach the capture, then captures one
// synthetic result: the collected items sorted stably and the total count.
export async function captureScan(
  ctx: Ctx,
  label: string,
  page: (last: unknown) => Promise<{ items: unknown[]; last: unknown }>,
): Promise<{ items: unknown[]; count: number }> {
  const seen: unknown[] = [];
  let last: unknown;
  for (let i = 0; i < 1000; i++) {
    const res = await page(last);
    seen.push(...res.items);
    last = res.last;
    if (last === undefined || last === null) break;
  }
  const items = sortItems(seen);
  const result = { items, count: items.length };
  ctx.capture(label, result);
  return result;
}

// captureWhenReady polls `probe` with plain sends (not captured) until `ready`
// is true, then captures the final call once. On dynamoat the answer is
// immediate, so it is captured straight away.
export async function captureWhenReady(
  ctx: Ctx,
  what: string,
  probe: () => Promise<unknown>,
  ready: (res: unknown) => boolean,
  capMs = 120_000,
): Promise<unknown> {
  if (ctx.target.kind !== 'aws') return ctx.call(probe);
  const deadline = Date.now() + capMs;
  let res = await probe();
  while (!ready(res) && Date.now() < deadline) {
    await delay(250);
    res = await probe();
  }
  if (!ready(res)) throw new Error(`${what} did not become ready within ${capMs}ms`);
  return ctx.call(probe);
}

// callRetrying runs `call` with plain sends while it fails with one of
// `retryOn` (which real DynamoDB can throw while a table is settling), then
// captures the successful result once without re-sending it.
export async function callRetrying(
  ctx: Ctx,
  what: string,
  call: () => Promise<unknown>,
  retryOn: string[],
  capMs = 120_000,
): Promise<unknown> {
  if (ctx.target.kind !== 'aws') return ctx.call(call);
  const deadline = Date.now() + capMs;
  for (;;) {
    let res: unknown;
    try {
      res = await call();
    } catch (err) {
      if (!retryOn.includes(errName(err)) || Date.now() >= deadline) throw err;
      await delay(1000);
      continue;
    }
    // Capture exactly as ctx.call does, so the golden file has the same shape
    // whichever target recorded it.
    return ctx.call(async () => res);
  }
}

function errName(err: unknown): string {
  return (err as { name?: string } | null | undefined)?.name ?? '';
}

function delay(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}
