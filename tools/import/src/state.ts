import { promises as fs } from "node:fs";
import path from "node:path";
import type { Key } from "./types";

// The resume state records, per table and Scan segment, how far the copy got.
// A segment entry is:
//   - absent:   the segment has not started
//   - a key:    the LastEvaluatedKey of the last page that was written
//   - null:     the segment finished
// When every segment is finished the table is marked done and is skipped on a
// rerun.
export interface TableState {
  done: boolean;
  segments: Record<string, Key | null>;
}

export interface StateFile {
  version: 1;
  tables: Record<string, TableState>;
}

export function emptyState(): StateFile {
  return { version: 1, tables: {} };
}

export function tableState(state: StateFile, table: string): TableState {
  let t = state.tables[table];
  if (!t) {
    t = { done: false, segments: {} };
    state.tables[table] = t;
  }
  return t;
}

// loadState returns an empty state when the file does not exist, and fails on
// a file that exists but is not a valid state file (silently losing resume
// information would be worse than stopping).
export async function loadState(file: string): Promise<StateFile> {
  let raw: string;
  try {
    raw = await fs.readFile(file, "utf8");
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") {
      return emptyState();
    }
    throw err;
  }
  const parsed: unknown = JSON.parse(raw);
  if (
    typeof parsed !== "object" ||
    parsed === null ||
    (parsed as { version?: unknown }).version !== 1 ||
    typeof (parsed as { tables?: unknown }).tables !== "object" ||
    (parsed as { tables?: unknown }).tables === null
  ) {
    throw new Error(`state file ${file} is not a dynamoat-import state file`);
  }
  return parsed as StateFile;
}

// StateWriter serializes writes so concurrent segments never interleave, and
// writes atomically (temp file, then rename) so a crash leaves the previous
// complete state, never a half-written one.
export class StateWriter {
  private queue: Promise<void> = Promise.resolve();

  constructor(private readonly file: string) {}

  save(state: StateFile): Promise<void> {
    // Stringify synchronously so the snapshot is consistent with the in-memory
    // mutation that triggered this save; only the file I/O is queued.
    const data = JSON.stringify(state, null, 2);
    const file = this.file;
    this.queue = this.queue.then(async () => {
      await fs.mkdir(path.dirname(file), { recursive: true });
      const tmp = path.join(
        path.dirname(file),
        `.${path.basename(file)}.tmp-${process.pid}`,
      );
      await fs.writeFile(tmp, data);
      await fs.rename(tmp, file);
    });
    return this.queue;
  }
}
