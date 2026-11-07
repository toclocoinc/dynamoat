import type { Item } from "./types";

export interface RetryOptions {
  // maxAttempts is the number of BatchWriteItem calls made per chunk before
  // giving up on its UnprocessedItems.
  maxAttempts: number;
  baseDelayMs: number;
  maxDelayMs: number;
}

export const defaultRetryOptions: RetryOptions = {
  maxAttempts: 10,
  baseDelayMs: 100,
  maxDelayMs: 10_000,
};

// The BatchWriteItem request limit.
export const batchChunkSize = 25;

export function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// backoff is full jitter on an exponential delay that doubles each attempt and
// is capped at maxDelayMs: attempt 1 waits up to 100 ms, then 200, 400, ...
// capping at 10 s.
function backoff(opts: RetryOptions, attempt: number, rng: () => number): number {
  const ceiling = Math.min(opts.baseDelayMs * 2 ** (attempt - 1), opts.maxDelayMs);
  return Math.floor(ceiling * rng());
}

// batchWriteWithRetry writes items in chunks of 25, retrying each chunk's
// UnprocessedItems with exponential backoff and jitter until it is empty or
// maxAttempts calls have been made. write takes a chunk and returns the items
// the service did not process. sleepFn and rng are injectable for tests.
export async function batchWriteWithRetry(
  write: (items: Item[]) => Promise<Item[]>,
  items: Item[],
  opts: RetryOptions = defaultRetryOptions,
  sleepFn: (ms: number) => Promise<void> = sleep,
  rng: () => number = Math.random,
): Promise<void> {
  for (let i = 0; i < items.length; i += batchChunkSize) {
    let pending = items.slice(i, i + batchChunkSize);
    for (let attempt = 1; ; attempt++) {
      const remaining = await write(pending);
      if (remaining.length === 0) {
        break;
      }
      if (attempt >= opts.maxAttempts) {
        throw new Error(
          `BatchWriteItem still had ${remaining.length} unprocessed item(s) after ${opts.maxAttempts} attempts`,
        );
      }
      await sleepFn(backoff(opts, attempt, rng));
      pending = remaining;
    }
  }
}
