// An HDR-style histogram written for the benchmark (no dependency).
//
// Values are recorded in integer nanoseconds into 256 sub-buckets per
// power-of-two range, HdrHistogram's "2 significant digits" layout: the
// worst-case relative error is 1/128 (under 1 %). Recording in nanoseconds
// keeps sub-millisecond latency meaningful. The public API is in milliseconds.

const SUB_BUCKET_COUNT = 256;
const SUB_BUCKET_HALF_COUNT = 128;
const SUB_BUCKET_HALF_MAGNITUDE = 7; // log2(SUB_BUCKET_HALF_COUNT)
const SUB_BUCKET_MASK = SUB_BUCKET_COUNT - 1;

// bitLength is the position of the highest set bit plus one (0 for 0).
function bitLength(n: number): number {
  if (n === 0) return 0;
  return Math.floor(Math.log2(n)) + 1;
}

// valueFromIndex is the lowest value that falls in a given counts-array slot.
function valueFromIndex(index: number): number {
  let bucketIndex = Math.floor(index / SUB_BUCKET_HALF_COUNT) - 1;
  let subBucketIndex = (index % SUB_BUCKET_HALF_COUNT) + SUB_BUCKET_HALF_COUNT;
  if (bucketIndex < 0) {
    subBucketIndex -= SUB_BUCKET_HALF_COUNT;
    bucketIndex = 0;
  }
  return subBucketIndex * 2 ** bucketIndex;
}

export class Histogram {
  #counts = new Map<number, number>();
  #total = 0;
  #maxMs = 0;

  recordMs(ms: number): void {
    if (!Number.isFinite(ms)) return;
    if (ms > this.#maxMs) this.#maxMs = ms;

    const value = Math.max(0, Math.round(ms * 1e6)); // milliseconds -> nanoseconds
    // HdrHistogram derives the bucket from value | subBucketMask; doing the
    // same with Math.max avoids 32-bit bitwise truncation for large values.
    const pow2ceiling = Math.max(bitLength(value), SUB_BUCKET_HALF_MAGNITUDE + 1);
    const bucketIndex = pow2ceiling - (SUB_BUCKET_HALF_MAGNITUDE + 1);
    const subBucketIndex = Math.floor(value / 2 ** bucketIndex) % SUB_BUCKET_COUNT;
    const index = (bucketIndex + 1) * SUB_BUCKET_HALF_COUNT + (subBucketIndex - SUB_BUCKET_HALF_COUNT);

    this.#counts.set(index, (this.#counts.get(index) ?? 0) + 1);
    this.#total++;
  }

  get totalCount(): number {
    return this.#total;
  }

  get maxMs(): number {
    return this.#maxMs;
  }

  // percentileMs interpolates within the sub-bucket that contains the p-th
  // percentile rank, keeping the estimate inside the bucket's ~1 % width.
  percentileMs(p: number): number {
    if (this.#total === 0) return 0;
    const pct = Math.min(100, Math.max(0, p));
    let target = Math.round((pct / 100) * this.#total);
    if (target < 1) target = 1;

    const indexes = [...this.#counts.keys()].sort((a, b) => a - b);
    let cumulative = 0;
    for (const index of indexes) {
      const count = this.#counts.get(index)!;
      cumulative += count;
      if (cumulative >= target) {
        const before = cumulative - count;
        const lower = valueFromIndex(index);
        const width = valueFromIndex(index + 1) - lower;
        return (lower + ((target - before) / count) * width) / 1e6;
      }
    }

    const last = indexes[indexes.length - 1];
    return valueFromIndex(last + 1) / 1e6;
  }
}
