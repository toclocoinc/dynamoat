// ArgError is a command-line usage error; the CLI prints it with the usage
// text and exits with status 2.
export class ArgError extends Error {}

export interface CliArgs {
  sourceEndpoint?: string;
  sourceRegion: string;
  targetEndpoint: string;
  targetRegion: string;
  tables?: string[];
  prefix?: string;
  segments: number;
  state: string;
  verify: boolean;
  dryRun: boolean;
}

export const usage = `\
Usage: dynamoat-import --source-endpoint <url> --source-region <region> \\
          --target-endpoint <url> --target-region <region> \\
          (--tables a,b | --prefix p) [--segments 8] [--state import-state.json] \\
          [--verify] [--dry-run]

  --source-endpoint <url>   source DynamoDB endpoint (omit for the real AWS service)
  --source-region <region>  source region (required)
  --target-endpoint <url>   target dynamoat endpoint (required)
  --target-region <region>  target region (required)
  --tables a,b              comma-separated table names to copy
  --prefix p                copy every source table whose name starts with p
  --segments n              parallel Scan segments (default 8)
  --state file              resume state file (default import-state.json)
  --verify                  compare source and target checksums after the copy
  --dry-run                 print the plan without changing the target`;

export function parseArgs(argv: string[]): CliArgs {
  const out: CliArgs = {
    sourceRegion: "",
    targetEndpoint: "",
    targetRegion: "",
    segments: 8,
    state: "import-state.json",
    verify: false,
    dryRun: false,
  };

  let i = 0;
  const takeValue = (name: string, inline?: string): string => {
    if (inline !== undefined) {
      return inline;
    }
    if (i + 1 >= argv.length) {
      throw new ArgError(`option ${name} requires a value`);
    }
    i += 1;
    return argv[i];
  };

  for (; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === "--") {
      if (i + 1 < argv.length) {
        throw new ArgError(`unexpected argument: ${argv[i + 1]}`);
      }
      break;
    }
    if (!arg.startsWith("--")) {
      throw new ArgError(`unexpected argument: ${arg}`);
    }
    let name = arg;
    let inline: string | undefined;
    const eq = arg.indexOf("=");
    if (eq !== -1) {
      name = arg.slice(0, eq);
      inline = arg.slice(eq + 1);
    }
    switch (name) {
      case "--source-endpoint":
        out.sourceEndpoint = takeValue(name, inline);
        break;
      case "--source-region":
        out.sourceRegion = takeValue(name, inline);
        break;
      case "--target-endpoint":
        out.targetEndpoint = takeValue(name, inline);
        break;
      case "--target-region":
        out.targetRegion = takeValue(name, inline);
        break;
      case "--tables":
        out.tables = splitTables(takeValue(name, inline));
        break;
      case "--prefix":
        out.prefix = takeValue(name, inline);
        break;
      case "--segments":
        out.segments = parseSegments(takeValue(name, inline));
        break;
      case "--state":
        out.state = takeValue(name, inline);
        break;
      case "--verify":
        rejectInline(name, inline);
        out.verify = true;
        break;
      case "--dry-run":
        rejectInline(name, inline);
        out.dryRun = true;
        break;
      default:
        throw new ArgError(`unknown option: ${name}`);
    }
  }

  if (!out.sourceRegion) {
    throw new ArgError("--source-region is required");
  }
  if (!out.targetEndpoint) {
    throw new ArgError("--target-endpoint is required");
  }
  if (!out.targetRegion) {
    throw new ArgError("--target-region is required");
  }
  if ((out.tables === undefined) === (out.prefix === undefined)) {
    throw new ArgError("specify exactly one of --tables or --prefix");
  }
  return out;
}

function splitTables(value: string): string[] {
  const tables = value
    .split(",")
    .map((t) => t.trim())
    .filter((t) => t.length > 0);
  if (tables.length === 0) {
    throw new ArgError("--tables must name at least one table");
  }
  return tables;
}

function parseSegments(value: string): number {
  const n = Number(value);
  if (!Number.isInteger(n) || n < 1) {
    throw new ArgError(`--segments must be a positive integer, got "${value}"`);
  }
  return n;
}

function rejectInline(name: string, inline: string | undefined): void {
  if (inline !== undefined) {
    throw new ArgError(`option ${name} does not take a value`);
  }
}
