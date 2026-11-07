import type {
  Item,
  Key,
  ScanPage,
  ScanRequest,
  TableDescription,
  TTLStatus,
} from "./types";

// TableReader reads a source table: its definition, TTL status and items.
// TableWriter recreates the definition on the target and writes items. Both
// are scannable so --verify can compare the two sides. The import logic
// depends only on these interfaces, which is what makes it unit-testable with
// in-memory fakes.
export interface TableReader {
  listTables(): Promise<string[]>;
  describeTable(name: string): Promise<TableDescription>;
  describeTimeToLive(name: string): Promise<TTLStatus>;
  scan(req: ScanRequest): Promise<ScanPage>;
}

export interface TableWriter {
  createTable(table: TableDescription): Promise<void>;
  // describeTable returns undefined when the table does not exist.
  describeTable(name: string): Promise<TableDescription | undefined>;
  updateTimeToLive(name: string, attributeName: string, enabled: boolean): Promise<void>;
  // writeItems writes every item, retrying UnprocessedItems internally.
  writeItems(name: string, items: Item[]): Promise<void>;
  scan(req: ScanRequest): Promise<ScanPage>;
}

// A LastEvaluatedKey is a Key; the alias keeps state-file code readable.
export type ResumeKey = Key;
