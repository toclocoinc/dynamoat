import { ArgError, parseArgs, usage } from "./args";
import { createClient } from "./clients";
import { DynamoTableReader } from "./source";
import { DynamoTableWriter } from "./target";
import { importTables } from "./import";
import { errorMessage } from "./errors";

export async function main(argv: string[]): Promise<number> {
  let args;
  try {
    args = parseArgs(argv);
  } catch (err) {
    if (err instanceof ArgError) {
      process.stderr.write(`${err.message}\n\n${usage}\n`);
      return 2;
    }
    throw err;
  }

  const reader = new DynamoTableReader(
    createClient({
      region: args.sourceRegion,
      ...(args.sourceEndpoint !== undefined ? { endpoint: args.sourceEndpoint } : {}),
    }),
  );

  // The target always signs with the DYNAMOAT_* credentials (empty when unset,
  // which is right for DYNAMOAT_AUTH=none) and never falls back to the ambient
  // AWS chain.
  const writer = new DynamoTableWriter(
    createClient({
      region: args.targetRegion,
      endpoint: args.targetEndpoint,
      credentials: {
        accessKeyId: process.env.DYNAMOAT_ACCESS_KEY_ID ?? "",
        secretAccessKey: process.env.DYNAMOAT_SECRET_ACCESS_KEY ?? "",
      },
    }),
  );

  let tables = args.tables;
  if (tables === undefined) {
    const names = await reader.listTables();
    tables = names.filter((n) => n.startsWith(args.prefix!)).sort();
    if (tables.length === 0) {
      process.stderr.write(`no source tables match prefix "${args.prefix}"\n`);
      return 1;
    }
  }

  const summary = await importTables({
    reader,
    writer,
    tables,
    segments: args.segments,
    statePath: args.state,
    verify: args.verify,
    dryRun: args.dryRun,
    progress: (line) => process.stderr.write(`${line}\n`),
  });

  process.stdout.write(`${JSON.stringify(summary, null, 2)}\n`);
  return summary.ok ? 0 : 1;
}

// This module is the CLI entry point: it is loaded by bin/dynamoat-import.js
// (through the tsx loader) or run directly with "tsx src/cli.ts".
try {
  process.exitCode = await main(process.argv.slice(2));
} catch (err) {
  process.stderr.write(`dynamoat-import: ${errorMessage(err)}\n`);
  process.exitCode = 1;
}
