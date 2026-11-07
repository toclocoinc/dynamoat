import { formatChecksum, totalChecksum } from "./checksum";
import type { TableReader, TableWriter } from "./interfaces";
import type { Key, ScanPage, ScanRequest } from "./types";

export interface VerifyResult {
  sourceCount: number;
  targetCount: number;
  sourceChecksum: string;
  targetChecksum: string;
  match: boolean;
}

// scanTotal streams a full table scan, accumulating the count and the mod-2^64
// checksum without holding the whole table in memory.
async function scanTotal(
  scan: (req: ScanRequest) => Promise<ScanPage>,
  table: string,
): Promise<{ count: number; checksum: bigint }> {
  const MASK = 0xffffffffffffffffn;
  let count = 0;
  let checksum = 0n;
  let startKey: Key | undefined;
  do {
    const page = await scan({ table, startKey, limit: 1000 });
    checksum = (checksum + totalChecksum(page.items)) & MASK;
    count += page.items.length;
    startKey = page.lastEvaluatedKey;
  } while (startKey);
  return { count, checksum };
}

// verifyTable scans both sides and compares item counts and an
// order-independent checksum.
export async function verifyTable(
  reader: TableReader,
  writer: TableWriter,
  table: string,
): Promise<VerifyResult> {
  const [source, target] = await Promise.all([
    scanTotal((req) => reader.scan(req), table),
    scanTotal((req) => writer.scan(req), table),
  ]);
  return {
    sourceCount: source.count,
    targetCount: target.count,
    sourceChecksum: formatChecksum(source.checksum),
    targetChecksum: formatChecksum(target.checksum),
    match: source.count === target.count && source.checksum === target.checksum,
  };
}
