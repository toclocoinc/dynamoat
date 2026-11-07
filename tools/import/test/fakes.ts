import type { TableReader, TableWriter } from "../src/interfaces";
import type {
  Item,
  Key,
  ScanPage,
  ScanRequest,
  TableDescription,
  TTLStatus,
} from "../src/types";

export function makeItem(pk: string, sk?: string): Item {
  const item: Item = { pk: { S: pk } };
  if (sk !== undefined) {
    item.sk = { S: sk };
  }
  return item;
}

export function basicDesc(name = "t"): TableDescription {
  return {
    TableName: name,
    TableStatus: "ACTIVE",
    KeySchema: [{ AttributeName: "pk", KeyType: "HASH" }],
    AttributeDefinitions: [{ AttributeName: "pk", AttributeType: "S" }],
  };
}

export function keyOf(item: Item, desc: TableDescription): Key {
  const key: Key = {};
  for (const el of desc.KeySchema) {
    key[el.AttributeName] = item[el.AttributeName];
  }
  return key;
}

function cmp(a: Item, b: Item): number {
  const pa = String(a.pk?.S ?? "");
  const pb = String(b.pk?.S ?? "");
  if (pa !== pb) {
    return pa < pb ? -1 : 1;
  }
  const sa = String(a.sk?.S ?? "");
  const sb = String(b.sk?.S ?? "");
  return sa < sb ? -1 : sa > sb ? 1 : 0;
}

// FakeReader serves a fixed item set, splitting it across segments by index and
// paginating at the requested limit, so resume behaviour can be exercised.
export class FakeReader implements TableReader {
  constructor(
    public readonly items: Item[],
    public readonly desc: TableDescription = basicDesc(),
    public readonly ttl: TTLStatus = { enabled: false },
  ) {}

  async listTables(): Promise<string[]> {
    return [this.desc.TableName];
  }

  async describeTable(): Promise<TableDescription> {
    return this.desc;
  }

  async describeTimeToLive(): Promise<TTLStatus> {
    return this.ttl;
  }

  async scan(req: ScanRequest): Promise<ScanPage> {
    const total = req.totalSegments ?? 1;
    const seg = req.segment ?? 0;
    const limit = req.limit ?? 1000;
    const sorted = [...this.items].sort(cmp);
    const mine = sorted.filter((_, i) => i % total === seg);
    const after = req.startKey ? mine.filter((it) => cmp(it, req.startKey!) > 0) : mine;
    const page = after.slice(0, limit);
    const rest = after.slice(limit);
    return {
      items: page,
      lastEvaluatedKey: rest.length > 0 ? keyOf(page[page.length - 1], this.desc) : undefined,
    };
  }
}

// FakeWriter records everything it is asked to do so tests can assert on it.
export class FakeWriter implements TableWriter {
  public readonly tables = new Map<string, TableDescription>();
  public readonly written: Item[] = [];
  public readonly ttlUpdates: { name: string; attributeName: string; enabled: boolean }[] = [];
  public readonly createCalls: TableDescription[] = [];

  async createTable(table: TableDescription): Promise<void> {
    this.createCalls.push(table);
    this.tables.set(table.TableName, { ...table, TableStatus: "ACTIVE" });
  }

  async describeTable(name: string): Promise<TableDescription | undefined> {
    return this.tables.get(name);
  }

  async updateTimeToLive(name: string, attributeName: string, enabled: boolean): Promise<void> {
    this.ttlUpdates.push({ name, attributeName, enabled });
  }

  async writeItems(name: string, items: Item[]): Promise<void> {
    this.written.push(...items);
  }

  async scan(req: ScanRequest): Promise<ScanPage> {
    const total = req.totalSegments ?? 1;
    const seg = req.segment ?? 0;
    const limit = req.limit ?? 1000;
    const desc = this.tables.get(req.table) ?? basicDesc(req.table);
    const sorted = [...this.written].sort(cmp);
    const mine = sorted.filter((_, i) => i % total === seg);
    const after = req.startKey ? mine.filter((it) => cmp(it, req.startKey!) > 0) : mine;
    const page = after.slice(0, limit);
    const rest = after.slice(limit);
    return {
      items: page,
      lastEvaluatedKey: rest.length > 0 ? keyOf(page[page.length - 1], desc) : undefined,
    };
  }
}
