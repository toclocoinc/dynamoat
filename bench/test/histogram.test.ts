import assert from "node:assert/strict";
import { test } from "node:test";
import { Histogram } from "../src/histogram.ts";

// referencePercentile is the sorted-array percentile the histogram is checked
// against, using the same "round to the nearest rank" convention.
function referencePercentile(sorted: number[], p: number): number {
  const n = sorted.length;
  let rank = Math.round((p / 100) * n);
  if (rank < 1) rank = 1;
  if (rank > n) rank = n;
  return sorted[rank - 1];
}

test("percentiles match a sorted-array reference within 1 %", () => {
  const n = 100_000;
  const values: number[] = [];
  const h = new Histogram();
  for (let i = 0; i < n; i++) {
    // Log-uniform across 0.01 ms to 60 s exercises many buckets.
    const ms = 10 ** (Math.random() * 6.78 - 2);
    values.push(ms);
    h.recordMs(ms);
  }
  values.sort((a, b) => a - b);

  for (const p of [10, 50, 90, 95, 99, 99.9, 99.99]) {
    const got = h.percentileMs(p);
    const want = referencePercentile(values, p);
    const tolerance = 0.01 * want + 1e-6;
    assert.ok(
      Math.abs(got - want) <= tolerance,
      `p${p}: got ${got} ms, want ${want} ms (tolerance ${tolerance})`,
    );
  }
});

test("maxMs and totalCount track the recorded values", () => {
  const h = new Histogram();
  h.recordMs(0.5);
  h.recordMs(120);
  h.recordMs(3.7);
  assert.equal(h.maxMs, 120);
  assert.equal(h.totalCount, 3);
});

test("percentiles are monotonic and an empty histogram is zero", () => {
  const empty = new Histogram();
  assert.equal(empty.percentileMs(50), 0);
  assert.equal(empty.percentileMs(99.9), 0);

  const h = new Histogram();
  for (const ms of [1, 2, 5, 10, 20, 50, 100]) h.recordMs(ms);
  const p50 = h.percentileMs(50);
  const p90 = h.percentileMs(90);
  const p99 = h.percentileMs(99);
  assert.ok(p50 <= p90 && p90 <= p99);
});
