// Closed-loop benchmark driver: each worker does one request at a time for
// the measurement window, after the warmup window, and every completed
// request is recorded.
import { performance } from "node:perf_hooks";
import { Histogram } from "./histogram.ts";
import { errorName } from "./util.ts";
import type { Scenario, ScenarioCtx } from "./scenarios.ts";

export interface RunOptions {
  ctx: ScenarioCtx;
  scenario: Scenario;
  concurrency: number;
  warmupMs: number;
  durationMs: number;
}

export interface RunResult {
  name: string;
  requests: number;
  errors: Record<string, number>;
  durationMs: number;
  histogram: Histogram;
}

export async function runScenario(opts: RunOptions): Promise<RunResult> {
  const { ctx, scenario, concurrency, warmupMs, durationMs } = opts;
  const histogram = new Histogram();
  const errors = new Map<string, number>();
  let requests = 0;

  const start = Date.now();
  const warmUntil = start + warmupMs;
  const measureUntil = warmUntil + durationMs;

  const worker = async (): Promise<void> => {
    while (Date.now() < measureUntil) {
      const t0 = performance.now();
      let err: unknown;
      try {
        await scenario.run(ctx);
      } catch (e) {
        err = e;
      }
      if (Date.now() >= warmUntil) {
        requests++;
        histogram.recordMs(performance.now() - t0);
        if (err !== undefined) {
          const name = errorName(err);
          errors.set(name, (errors.get(name) ?? 0) + 1);
        }
      }
    }
  };

  await Promise.all(Array.from({ length: concurrency }, () => worker()));

  return {
    name: scenario.name,
    requests,
    errors: Object.fromEntries(errors),
    durationMs: Math.max(1, Date.now() - warmUntil),
    histogram,
  };
}
