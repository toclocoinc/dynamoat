// Small shared helpers with no dependencies.

export function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// errorName is the AWS SDK's short error name (err.name), falling back to
// "Error" for anything that is not an SDK-shaped error.
export function errorName(e: unknown): string {
  if (e && typeof e === "object" && "name" in e && typeof (e as { name: unknown }).name === "string") {
    return (e as { name: string }).name;
  }
  return "Error";
}

// parseDuration accepts a bare number of seconds or a value with an "s" or
// "ms" suffix, matching the CLI's --duration/--warmup flags.
export function parseDuration(s: string): number {
  const m = /^(\d+)(ms|s)?$/.exec(s.trim());
  if (!m) throw new Error(`invalid duration: ${s} (expected e.g. 60s or 500ms)`);
  const n = Number(m[1]);
  return m[2] === "ms" ? n : n * 1000;
}
