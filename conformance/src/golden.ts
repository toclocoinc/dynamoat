// Golden files: the recorded, normalised result of each test case. Recorded
// with CONFORMANCE_RECORD=1 against real DynamoDB, then compared against the
// normalised dynamoat result.

import { existsSync } from 'node:fs';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
export const goldenDir = join(here, '..', 'golden');

export function goldenPath(name: string): string {
  return join(goldenDir, `${name}.json`);
}

export function goldenExists(name: string): boolean {
  return existsSync(goldenPath(name));
}

export async function writeGolden(name: string, value: unknown): Promise<void> {
  await mkdir(goldenDir, { recursive: true });
  await writeFile(goldenPath(name), JSON.stringify(value, null, 2) + '\n');
}

export async function readGolden(name: string): Promise<unknown> {
  return JSON.parse(await readFile(goldenPath(name), 'utf8'));
}

// diff returns human-readable lines describing the first differences between
// expected and actual. Empty means the two are equal.
export function diff(expected: unknown, actual: unknown, path = '$'): string[] {
  if (Object.is(expected, actual)) return [];

  const te = typeof expected;
  const ta = typeof actual;
  if (te !== ta || expected === null || actual === null || (te !== 'object' && te !== 'function')) {
    return [`${path}: expected ${describe(expected)}, got ${describe(actual)}`];
  }

  const eArr = Array.isArray(expected);
  const aArr = Array.isArray(actual);
  if (eArr !== aArr) {
    return [`${path}: expected ${eArr ? 'array' : 'object'}, got ${aArr ? 'array' : 'object'}`];
  }
  if (eArr) {
    const e = expected as unknown[];
    const a = actual as unknown[];
    const out: string[] = [];
    const len = Math.max(e.length, a.length);
    for (let i = 0; i < len; i++) {
      if (!(i in e)) {
        out.push(`${path}[${i}]: expected <absent>, got ${describe(a[i])}`);
        continue;
      }
      if (!(i in a)) {
        out.push(`${path}[${i}]: expected ${describe(e[i])}, got <absent>`);
        continue;
      }
      out.push(...diff(e[i], a[i], `${path}[${i}]`));
    }
    return out;
  }

  const e = expected as Record<string, unknown>;
  const a = actual as Record<string, unknown>;
  const out: string[] = [];
  const keys = [...new Set([...Object.keys(e), ...Object.keys(a)])].sort();
  for (const k of keys) {
    if (!(k in e)) {
      out.push(`${path}.${k}: expected <absent>, got ${describe(a[k])}`);
      continue;
    }
    if (!(k in a)) {
      out.push(`${path}.${k}: expected ${describe(e[k])}, got <absent>`);
      continue;
    }
    out.push(...diff(e[k], a[k], `${path}.${k}`));
  }
  return out;
}

function describe(v: unknown): string {
  if (v === undefined) return 'undefined';
  try {
    return JSON.stringify(v);
  } catch {
    return String(v);
  }
}
