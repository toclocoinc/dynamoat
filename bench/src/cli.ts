// dynamoat-bench: measure dynamoat and DynamoDB with the same workload.
import { randomBytes } from "node:crypto";
import { writeFileSync } from "node:fs";
import os from "node:os";
import { makeDocClient } from "./client.ts";
import { boardCount } from "./items.ts";
import { buildReport, printTable, type ScenarioReport } from "./report.ts";
import { runScenario } from "./run.ts";
import { scenarios, type BenchState } from "./scenarios.ts";
import { createTable, deleteTable, seed, waitActive } from "./table.ts";
import { parseDuration } from "./util.ts";

interface Options {
  endpoint?: string;
  region: string;
  scenario: string;
  concurrency: number;
  durationMs: number;
  warmupMs: number;
  seedItems: number;
  keep: boolean;
  out?: string;
}

const SCENARIO_NAMES = scenarios.map((s) => s.name);

function parseArgs(argv: string[]): Options {
  const opts: Options = {
    region: "us-east-1",
    scenario: "",
    concurrency: 32,
    durationMs: 60_000,
    warmupMs: 10_000,
    seedItems: 10_000,
    keep: false,
  };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    const value = (flag: string): string => {
      const v = argv[i + 1];
      if (v === undefined) throw new Error(`${flag} needs a value`);
      i++;
      return v;
    };
    switch (a) {
      case "--endpoint":
        opts.endpoint = value(a);
        break;
      case "--region":
        opts.region = value(a);
        break;
      case "--scenario":
        opts.scenario = value(a);
        break;
      case "--concurrency":
        opts.concurrency = Number(value(a));
        break;
      case "--duration":
        opts.durationMs = parseDuration(value(a));
        break;
      case "--warmup":
        opts.warmupMs = parseDuration(value(a));
        break;
      case "--seed-items":
        opts.seedItems = Number(value(a));
        break;
      case "--keep":
        opts.keep = true;
        break;
      case "--out":
        opts.out = value(a);
        break;
      case "--help":
        printHelp();
        process.exit(0);
      default:
        throw new Error(`unknown argument: ${a}`);
    }
  }
  return opts;
}

function printHelp(): void {
  process.stdout.write(`Usage: dynamoat-bench --scenario <name|all> [options]

Options:
  --endpoint <url>     dynamoat or DynamoDB Local endpoint; omit for real DynamoDB
  --region <r>         AWS region (default us-east-1)
  --scenario <name|all>  get | query-gsi | update-7gsi | put | all
  --concurrency <n>    closed-loop workers (default 32)
  --duration <t>       measurement window, e.g. 60s (default 60s)
  --warmup <t>         warmup before measurement (default 10s)
  --seed-items <n>     items to seed (default 10000)
  --keep               do not delete the table at the end
  --out <path>         write JSON results to this file
`);
}

function validate(opts: Options): void {
  if (!opts.scenario) throw new Error("--scenario is required (get | query-gsi | update-7gsi | put | all)");
  if (opts.scenario !== "all" && !SCENARIO_NAMES.includes(opts.scenario)) {
    throw new Error(`unknown scenario ${opts.scenario} (expected ${SCENARIO_NAMES.join(", ")} or all)`);
  }
  if (!Number.isInteger(opts.concurrency) || opts.concurrency < 1) {
    throw new Error("--concurrency must be a positive integer");
  }
  if (!Number.isInteger(opts.seedItems) || opts.seedItems < 0) {
    throw new Error("--seed-items must be a non-negative integer");
  }
}

function log(msg: string): void {
  process.stderr.write(`dynamoat-bench: ${msg}\n`);
}

async function main(): Promise<void> {
  const opts = parseArgs(process.argv.slice(2));
  validate(opts);

  const doc = makeDocClient(opts.endpoint, opts.region);
  const tableName = `bench-${randomBytes(6).toString("hex")}`;
  const names = opts.scenario === "all" ? SCENARIO_NAMES : [opts.scenario];
  const state: BenchState = {
    seedCount: opts.seedItems,
    boards: boardCount(opts.seedItems),
    nextPutId: 0,
    nextName: 0,
  };

  const reports: ScenarioReport[] = [];
  try {
    log(`creating table ${tableName}`);
    await createTable(doc, tableName);
    await waitActive(doc, tableName, 120_000, log);
    log(`seeding ${opts.seedItems} items`);
    await seed(doc, tableName, opts.seedItems, log);

    for (const name of names) {
      const scenario = scenarios.find((s) => s.name === name)!;
      log(`running ${name}: concurrency ${opts.concurrency}, warmup ${opts.warmupMs}ms, duration ${opts.durationMs}ms`);
      const result = await runScenario({
        ctx: { doc, tableName, state },
        scenario,
        concurrency: opts.concurrency,
        warmupMs: opts.warmupMs,
        durationMs: opts.durationMs,
      });
      reports.push(buildReport(result));
    }
  } finally {
    if (opts.keep) {
      log(`keeping table ${tableName}`);
    } else {
      log(`deleting table ${tableName}`);
      await deleteTable(doc, tableName, log);
    }
  }

  printTable(reports);

  if (opts.out) {
    const json = {
      tool: "dynamoat-bench",
      parameters: {
        endpoint: opts.endpoint ?? null,
        region: opts.region,
        scenario: opts.scenario,
        concurrency: opts.concurrency,
        durationMs: opts.durationMs,
        warmupMs: opts.warmupMs,
        seedItems: opts.seedItems,
        keep: opts.keep,
        tableName,
      },
      node: { version: process.version, cpuCount: os.cpus().length },
      scenarios: reports,
    };
    writeFileSync(opts.out, JSON.stringify(json, null, 2) + "\n");
    log(`wrote ${opts.out}`);
  }
}

main().catch((e) => {
  process.stderr.write(`dynamoat-bench: ${(e as Error).stack ?? e}\n`);
  process.exit(1);
});
