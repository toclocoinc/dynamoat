import test from "node:test";
import assert from "node:assert/strict";
import { batchWriteWithRetry, defaultRetryOptions } from "../src/batch";
import type { Item } from "../src/types";

function makeItems(n: number): Item[] {
  return Array.from({ length: n }, (_, i) => ({ pk: { N: String(i) } }));
}

test("chunks writes at 25 items", async () => {
  const calls: number[] = [];
  await batchWriteWithRetry(
    async (items) => {
      calls.push(items.length);
      return [];
    },
    makeItems(60),
    defaultRetryOptions,
    async () => {},
    Math.random,
  );
  assert.deepEqual(calls, [25, 25, 10]);
});

test("retries UnprocessedItems with doubling backoff and jitter", async () => {
  const sleeps: number[] = [];
  let attempts = 0;
  await batchWriteWithRetry(
    async (items) => {
      attempts += 1;
      // Fail the first three attempts, then accept everything.
      return attempts <= 3 ? [...items] : [];
    },
    makeItems(25),
    defaultRetryOptions,
    async (ms) => {
      sleeps.push(ms);
    },
    () => 0.5,
  );
  assert.equal(attempts, 4);
  assert.deepEqual(sleeps, [50, 100, 200]);
});

test("gives up after maxAttempts when items stay unprocessed", async () => {
  const sleeps: number[] = [];
  await assert.rejects(
    batchWriteWithRetry(
      async (items) => [...items],
      makeItems(25),
      defaultRetryOptions,
      async (ms) => {
        sleeps.push(ms);
      },
      () => 0,
    ),
    /10 attempts/,
  );
  // Nine waits: after attempts 1..9, the 10th fails without a wait.
  assert.equal(sleeps.length, 9);
});
