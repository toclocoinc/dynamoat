// isError reports whether err is an AWS SDK error with the given short name
// (err.name in the JS SDK, e.g. "ResourceNotFoundException").
export function isError(err: unknown, code: string): boolean {
  return (
    typeof err === "object" &&
    err !== null &&
    (err as { name?: unknown }).name === code
  );
}

// errorMessage renders an error for the stderr summary, falling back to the
// message itself when the shape is unknown.
export function errorMessage(err: unknown): string {
  if (err instanceof Error && err.message) {
    return err.message;
  }
  return String(err);
}
