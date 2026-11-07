// Assertion helpers for the conformance cases. Each names the case and shows
// the expected and actual values in its message, so a failure identifies the
// case and the mismatch even though no golden files are recorded yet.

import assert from 'node:assert/strict';

function message(caseName: string, what: string, expected: unknown, actual: unknown): string {
  return `${caseName}: ${what}: expected ${fmt(expected)}, got ${fmt(actual)}`;
}

function fmt(v: unknown): string {
  try {
    return JSON.stringify(v);
  } catch {
    return String(v);
  }
}

// eq asserts strict equality of two scalar values.
export function eq(caseName: string, actual: unknown, expected: unknown, what: string): void {
  assert.strictEqual(actual, expected, message(caseName, what, expected, actual));
}

// deep asserts deep strict equality.
export function deep(caseName: string, actual: unknown, expected: unknown, what: string): void {
  assert.deepStrictEqual(actual, expected, message(caseName, what, expected, actual));
}

// setEq compares two arrays as sets, ignoring order.
export function setEq(caseName: string, actual: unknown[], expected: unknown[], what: string): void {
  const a = [...actual].sort(cmp);
  const e = [...expected].sort(cmp);
  assert.deepStrictEqual(a, e, message(caseName, what, expected, actual));
}

function cmp(a: unknown, b: unknown): number {
  if (a < b) return -1;
  if (a > b) return 1;
  return 0;
}
