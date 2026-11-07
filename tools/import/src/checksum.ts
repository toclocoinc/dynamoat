import { createHash } from "node:crypto";
import type { Item } from "./types";

// The verify checksum is order-independent: the sum, mod 2^64, of a per-item
// digest. Each item's digest is the first 8 bytes (big-endian) of SHA-256 over
// its canonical JSON.
const MASK = 0xffffffffffffffffn;

// canonicalJson renders a DynamoDB item (or any part of one) with object keys
// sorted at every level. Attribute values are already strings, booleans or
// nulls in DynamoDB JSON, and numbers are their DynamoDB "N" strings, so
// JSON.stringify is deterministic once the keys are sorted.
export function canonicalJson(value: unknown): string {
  return JSON.stringify(canonical(value));
}

function canonical(value: unknown): unknown {
  if (Array.isArray(value)) {
    return value.map(canonical);
  }
  if (value !== null && typeof value === "object") {
    const source = value as Record<string, unknown>;
    const out: Record<string, unknown> = {};
    for (const key of Object.keys(source).sort()) {
      out[key] = canonical(source[key]);
    }
    return out;
  }
  return value;
}

// itemChecksum is one item's contribution to the total: the first 8 bytes of
// SHA-256(canonical JSON), read as an unsigned big-endian 64-bit integer.
export function itemChecksum(item: Item): bigint {
  const json = canonicalJson(item);
  const digest = createHash("sha256").update(json, "utf8").digest();
  return digest.readBigUInt64BE(0);
}

// totalChecksum sums the per-item checksums mod 2^64, so the order items are
// added in does not matter.
export function totalChecksum(items: Iterable<Item>): bigint {
  let total = 0n;
  for (const item of items) {
    total = (total + itemChecksum(item)) & MASK;
  }
  return total;
}

export function formatChecksum(n: bigint): string {
  return n.toString(10);
}
