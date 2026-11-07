import type { TableReader, TableWriter } from "./interfaces";
import { emptyState, loadState, StateWriter, tableState, type StateFile } from "./state";
import { isError } from "./errors";
import type { Key, KeySchemaElement, TableDescription } from "./types";
import { verifyTable, type VerifyResult } from "./verify";

export interface ImportOptions {
  reader: TableReader;
  writer: TableWriter;
  tables: string[];
  segments: number;
  statePath: string;
  verify: boolean;
  dryRun: boolean;
  // progress receives per-table progress lines for stderr.
  progress: (line: string) => void;
}

export interface TableResult {
  table: string;
  status: "created" | "resumed" | "dry-run" | "failed";
  items?: number;
  verify?: VerifyResult;
  error?: string;
}

export interface ImportSummary {
  tables: TableResult[];
  ok: boolean;
}

export async function importTables(opts: ImportOptions): Promise<ImportSummary> {
  const state = opts.dryRun ? emptyState() : await loadState(opts.statePath);
  const stateWriter = opts.dryRun ? null : new StateWriter(opts.statePath);

  const results: TableResult[] = [];
  let ok = true;

  for (const table of opts.tables) {
    const result = await importTable(opts, table, state, stateWriter);
    results.push(result);
    if (result.status === "failed") {
      ok = false;
    }
  }

  return { tables: results, ok };
}

async function importTable(
  opts: ImportOptions,
  table: string,
  state: StateFile,
  stateWriter: StateWriter | null,
): Promise<TableResult> {
  const { progress } = opts;

  const source = await opts.reader.describeTable(table);

  const ensure = await ensureTable(opts.writer, source, opts.dryRun, progress);
  if (ensure === "mismatch") {
    return {
      table,
      status: "failed",
      error: `target table "${table}" already exists with a different key schema`,
    };
  }
  const created = ensure === "created";

  if (opts.dryRun) {
    progress(`table ${table}: dry-run, would ${created ? "create" : "resume"} the target table`);
    return { table, status: "dry-run" };
  }

  await copyTTL(opts, source.TableName, progress);

  const ts = tableState(state, table);
  let copiedItems: number | undefined;
  if (ts.done) {
    progress(`table ${table}: already complete in state file, skipping copy`);
  } else {
    const start = Date.now();
    copiedItems = await copySegments(opts, table, ts, state, stateWriter!);
    ts.done = true;
    await stateWriter!.save(state);
    const elapsed = (Date.now() - start) / 1000;
    const rate = elapsed > 0 ? Math.round(copiedItems / elapsed) : copiedItems;
    progress(`table ${table}: copied ${copiedItems} items in ${elapsed.toFixed(1)}s (${rate} items/s)`);
  }

  let verify: VerifyResult | undefined;
  if (opts.verify) {
    verify = await verifyTable(opts.reader, opts.writer, table);
    progress(
      `table ${table}: verify source=${verify.sourceCount} (${verify.sourceChecksum}) target=${verify.targetCount} (${verify.targetChecksum}) ${verify.match ? "MATCH" : "MISMATCH"}`,
    );
  }

  const result: TableResult = {
    table,
    status: created ? "created" : "resumed",
    ...(copiedItems !== undefined ? { items: copiedItems } : {}),
    ...(opts.verify ? { verify } : {}),
  };
  if (opts.verify && verify && !verify.match) {
    result.status = "failed";
    result.error = "verify mismatch: source and target item counts or checksums differ";
  }
  return result;
}

// ensureTable recreates the source table on the target, or checks that an
// existing table matches. It returns "created" when the target table was
// created, "resumed" when it already existed with a matching key schema, and
// "mismatch" when it exists with a different key schema.
async function ensureTable(
  writer: TableWriter,
  source: TableDescription,
  dryRun: boolean,
  progress: (line: string) => void,
): Promise<"created" | "resumed" | "mismatch"> {
  const existing = await writer.describeTable(source.TableName);
  if (existing) {
    if (!keySchemasEqual(existing.KeySchema, source.KeySchema)) {
      return "mismatch";
    }
    progress(`table ${source.TableName}: target exists with matching key schema, resuming`);
    return "resumed";
  }
  if (dryRun) {
    return "created";
  }
  await writer.createTable(source);
  progress(`table ${source.TableName}: created target, waiting for ACTIVE`);
  await waitActive(writer, source.TableName);
  return "created";
}

async function waitActive(writer: TableWriter, name: string): Promise<void> {
  const deadline = Date.now() + 10 * 60 * 1000;
  for (;;) {
    const desc = await writer.describeTable(name);
    if (desc?.TableStatus === "ACTIVE") {
      return;
    }
    if (Date.now() >= deadline) {
      throw new Error(`table "${name}" did not become ACTIVE within 10 minutes`);
    }
    await new Promise((resolve) => setTimeout(resolve, 1000));
  }
}

async function copyTTL(
  opts: ImportOptions,
  table: string,
  progress: (line: string) => void,
): Promise<void> {
  let ttl;
  try {
    ttl = await opts.reader.describeTimeToLive(table);
  } catch (err) {
    if (isError(err, "UnknownOperationException")) {
      progress(`table ${table}: source does not support DescribeTimeToLive, skipping TTL`);
      return;
    }
    throw err;
  }
  if (!ttl.enabled) {
    return;
  }
  if (!ttl.attributeName) {
    progress(`table ${table}: TTL enabled on source without an attribute name, skipping TTL`);
    return;
  }
  try {
    await opts.writer.updateTimeToLive(table, ttl.attributeName, true);
  } catch (err) {
    if (isError(err, "UnknownOperationException")) {
      progress(`table ${table}: target does not support UpdateTimeToLive, skipping TTL`);
      return;
    }
    throw err;
  }
  progress(`table ${table}: enabled TTL on attribute "${ttl.attributeName}"`);
}

// copySegments runs the parallel Scan: one worker per segment, each following
// its own LastEvaluatedKey and writing pages with BatchWriteItem. After every
// page the segment's resume key is written to the state file, so a rerun
// continues where the copy stopped.
async function copySegments(
  opts: ImportOptions,
  table: string,
  ts: { segments: Record<string, Key | null> },
  state: StateFile,
  stateWriter: StateWriter,
): Promise<number> {
  const segments = Math.max(1, opts.segments);
  const counts = await Promise.all(
    Array.from({ length: segments }, (_, seg) =>
      copySegment(opts, table, String(seg), ts, state, stateWriter),
    ),
  );
  return counts.reduce((a, b) => a + b, 0);
}

async function copySegment(
  opts: ImportOptions,
  table: string,
  key: string,
  ts: { segments: Record<string, Key | null> },
  state: StateFile,
  stateWriter: StateWriter,
): Promise<number> {
  // null marks a segment already completed; skip it on a resume.
  if (ts.segments[key] === null) {
    return 0;
  }
  const segments = Math.max(1, opts.segments);
  const seg = Number(key);
  let startKey = ts.segments[key] as Key | undefined;
  let count = 0;
  for (;;) {
    const page = await opts.reader.scan({
      table,
      segment: seg,
      totalSegments: segments,
      startKey,
      limit: 1000,
    });
    if (page.items.length > 0) {
      await opts.writer.writeItems(table, page.items);
      count += page.items.length;
    }
    startKey = page.lastEvaluatedKey;
    ts.segments[key] = startKey ?? null;
    await stateWriter.save(state);
    if (!startKey) {
      break;
    }
  }
  return count;
}

function keySchemasEqual(a: KeySchemaElement[], b: KeySchemaElement[]): boolean {
  if (a.length !== b.length) {
    return false;
  }
  return a.every(
    (el, i) => el.AttributeName === b[i].AttributeName && el.KeyType === b[i].KeyType,
  );
}
