// Per-scenario numbers and the human-readable table.
import type { RunResult } from "./run.ts";

export interface LatencyMs {
  p50: number;
  p90: number;
  p99: number;
  p99_9: number;
  max: number;
}

export interface ScenarioReport {
  name: string;
  requests: number;
  throughput: number; // requests per second over the measured window
  errors: Record<string, number>;
  latencyMs: LatencyMs;
}

export function buildReport(result: RunResult): ScenarioReport {
  const seconds = result.durationMs / 1000;
  const h = result.histogram;
  return {
    name: result.name,
    requests: result.requests,
    throughput: result.requests / seconds,
    errors: result.errors,
    latencyMs: {
      p50: h.percentileMs(50),
      p90: h.percentileMs(90),
      p99: h.percentileMs(99),
      p99_9: h.percentileMs(99.9),
      max: h.maxMs,
    },
  };
}

function fmt(n: number): string {
  if (!Number.isFinite(n)) return "-";
  return n.toFixed(3);
}

function fmtCount(n: number): string {
  return String(Math.round(n));
}

function errorsSummary(errors: Record<string, number>): string {
  const entries = Object.entries(errors);
  if (entries.length === 0) return "-";
  return entries.map(([name, count]) => `${name}=${count}`).join(", ");
}

export function printTable(reports: ScenarioReport[]): void {
  const header = ["scenario", "requests", "throughput/s", "errors", "p50 ms", "p90 ms", "p99 ms", "p99.9 ms", "max ms"];
  const rows = reports.map((r) => [
    r.name,
    fmtCount(r.requests),
    fmt(r.throughput),
    errorsSummary(r.errors),
    fmt(r.latencyMs.p50),
    fmt(r.latencyMs.p90),
    fmt(r.latencyMs.p99),
    fmt(r.latencyMs.p99_9),
    fmt(r.latencyMs.max),
  ]);

  const widths = header.map((_, c) =>
    Math.max(header[c].length, ...rows.map((row) => row[c].length)),
  );

  const line = (cells: string[]): string =>
    cells.map((cell, c) => cell.padEnd(widths[c])).join("  ").trimEnd();

  process.stdout.write(line(header) + "\n");
  for (const row of rows) process.stdout.write(line(row) + "\n");
}
