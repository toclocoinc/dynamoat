// Normalisation turns a captured SDK result or error into a stable form:
// volatile fields (request IDs, timestamps, ARN account and region, table IDs,
// stream labels, the run's random table prefix) become fixed placeholders so
// dynamoat and real DynamoDB produce identical golden files.

export interface Capture {
  label?: string;
  result?: unknown;
  error?: unknown;
}

export interface NormaliserOptions {
  // prefix is the aws target's random table prefix ("conformance-<hex>-"),
  // stripped from every string so dynamoat (no prefix) matches aws (prefix).
  prefix?: string;
}

const PLACEHOLDER = '«normalised»';
const REQUEST_ID_RE = /requestid$/i;

export function makeNormaliser(opts: NormaliserOptions = {}): (value: unknown) => unknown {
  const prefix = opts.prefix ?? '';

  const walk = (value: unknown, key?: string, parentKey?: string): unknown => {
    if (value === null || value === undefined) return value;
    const t = typeof value;
    if (t === 'string') return normaliseString(value, key, prefix);
    if (t === 'number') return normaliseNumber(value, key);
    if (t === 'boolean' || t === 'bigint') return t === 'bigint' ? Number(value) : value;
    if (Buffer.isBuffer(value) || value instanceof Uint8Array) return toBase64(value);
    if (Array.isArray(value)) {
      const mapped = value.map((v) => walk(v, undefined, key));
      // Order DynamoDB does not define is not compared: indexes and replicas
      // are sorted by their names, and each BatchGetItem Responses[table] list
      // is sorted by a stable JSON form of its items.
      if (key === 'GlobalSecondaryIndexes' || key === 'LocalSecondaryIndexes') {
        return sortByField(mapped, 'IndexName');
      }
      if (key === 'Replicas') return sortByField(mapped, 'RegionName');
      if (key === 'Tags') return sortByField(mapped, 'Key');
      if (parentKey === 'Responses') return sortItems(mapped);
      return mapped;
    }
    if (t === 'object') {
      const out: Record<string, unknown> = {};
      for (const [k, v] of Object.entries(value as Record<string, unknown>)) {
        // Drop undefined so the structure survives a JSON round trip unchanged.
        if (v === undefined) continue;
        const nk = stripPrefix(k, prefix);
        out[nk] = walk(v, nk, key);
      }
      return out;
    }
    return value;
  };

  return walk;
}

function normaliseString(s: string, key: string | undefined, prefix: string): string {
  if (key !== undefined && REQUEST_ID_RE.test(key)) return PLACEHOLDER;
  if (key === 'TableId') return PLACEHOLDER;
  if (key === 'LatestStreamLabel' || key === 'StreamLabel') return PLACEHOLDER;
  if (key === 'NextToken') return PLACEHOLDER;
  let out = stripPrefix(s, prefix);
  if (out.startsWith('arn:aws:dynamodb:')) out = normaliseArn(out);
  return out;
}

function stripPrefix(s: string, prefix: string): string {
  return prefix ? s.split(prefix).join('') : s;
}

function normaliseNumber(n: number, key: string | undefined): number {
  if (
    key === 'CreationDateTime' ||
    key === 'LastUpdateToPayPerRequestDateTime' ||
    key === 'EarliestRestorableDateTime' ||
    key === 'LatestRestorableDateTime' ||
    key === 'TableCreationDateTime' ||
    key === 'BackupCreationDateTime' ||
    key === 'RestoreDateTime' ||
    // DynamoDB reports 0 until it computes a backup's size, hours later.
    key === 'BackupSizeBytes'
  ) {
    return 0;
  }
  return n;
}

// normaliseArn replaces the account id and region, and the stream label (a
// timestamp) inside a stream ARN. The table name inside the ARN has already had
// the run's prefix stripped.
function normaliseArn(s: string): string {
  const m = s.match(/^arn:aws:dynamodb:[^:]+:[^:]+:(.*)$/);
  if (!m) return s;
  let rest = m[1];
  rest = rest.replace(/^table\/([^/]+)(\/stream\/[0-9TZ:.+-]+)?$/, (_all, table, stream) => {
    return 'table/' + table + (stream ? '/stream/' + PLACEHOLDER : '');
  });
  rest = rest.replace(/^table\/([^/]+)\/backup\/[^/]+$/, (_all, table) => {
    return 'table/' + table + '/backup/' + PLACEHOLDER;
  });
  return 'arn:aws:dynamodb:' + PLACEHOLDER + ':' + PLACEHOLDER + ':' + rest;
}

function toBase64(buf: Uint8Array): string {
  return Buffer.from(buf).toString('base64');
}

// sortItems orders values whose order DynamoDB does not define, using a stable
// canonical JSON form so attribute order within an item does not matter.
export function sortItems(items: unknown[]): unknown[] {
  return [...items].sort((a, b) => {
    const ka = stableKey(a);
    const kb = stableKey(b);
    return ka < kb ? -1 : ka > kb ? 1 : 0;
  });
}

function sortByField(items: unknown[], field: string): unknown[] {
  return [...items].sort((a, b) => {
    const av = (a as Record<string, unknown> | undefined)?.[field];
    const bv = (b as Record<string, unknown> | undefined)?.[field];
    return String(av ?? '') < String(bv ?? '') ? -1 : String(av ?? '') > String(bv ?? '') ? 1 : 0;
  });
}

function stableKey(value: unknown): string {
  return JSON.stringify(canonical(value));
}

function canonical(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(canonical);
  if (Buffer.isBuffer(value) || value instanceof Uint8Array) return toBase64(value);
  if (value !== null && typeof value === 'object') {
    const out: Record<string, unknown> = {};
    for (const k of Object.keys(value as Record<string, unknown>).sort()) {
      out[k] = canonical((value as Record<string, unknown>)[k]);
    }
    return out;
  }
  return value;
}

// cleanResult keeps the response but reduces $metadata to its stable part, so a
// later normalise pass can act on the body. Binary values become base64 so they
// survive JSON serialisation into golden files.
export function cleanResult(res: unknown): unknown {
  if (res === null || typeof res !== 'object') return res;
  const out: Record<string, unknown> = { ...(res as Record<string, unknown>) };
  const md = (res as Record<string, unknown>).$metadata;
  out.$metadata =
    md && typeof md === 'object' ? { httpStatusCode: (md as Record<string, unknown>).httpStatusCode } : {};
  return out;
}

// cleanError keeps the fields the conformance suite compares: error name,
// message, HTTP status, and the extras DynamoDB attaches (CancellationReasons,
// Item).
export function cleanError(err: unknown): unknown {
  const e = err as Record<string, unknown>;
  const out: Record<string, unknown> = {
    name: e?.name,
    message: e?.message,
    $metadata: { httpStatusCode: (e?.$metadata as Record<string, unknown> | undefined)?.httpStatusCode },
  };
  if (e?.CancellationReasons !== undefined) out.CancellationReasons = e.CancellationReasons;
  if (e?.Item !== undefined) out.Item = e.Item;
  return out;
}
