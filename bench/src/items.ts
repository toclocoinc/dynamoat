// Tenant-shaped test data: a tenant table keyed by id, with seven
// global secondary indexes, and BACKLOG-like items whose
// comboKey is SUB:<s>|KBN:<board>|ID:<id>. Items are plain JavaScript objects
// as @aws-sdk/lib-dynamodb accepts them.

export const ITEMS_PER_BOARD = 100;
export const STATUSES = ["open", "blocked", "in-progress", "done"] as const;

// PAYLOAD pads every item to 1-2 KB. It is shared and deterministic so the
// generator is cheap to test.
const PAYLOAD = "backlog-filler-".repeat(85).slice(0, 1200);

export type Item = Record<string, unknown>;

export function subscriptionFor(board: number): string {
  return `sub-${board}`;
}

export function comboKeyFor(subscription: string, board: number, id: string): string {
  return `SUB:${subscription}|KBN:board-${board}|ID:${id}`;
}

// comboKeyPrefix is the begins_with operand that selects one board (100 items).
export function comboKeyPrefix(subscription: string, board: number): string {
  return `SUB:${subscription}|KBN:board-${board}|`;
}

export function boardCount(seedItems: number): number {
  return Math.ceil(seedItems / ITEMS_PER_BOARD);
}

// makeSeedItem builds the seeded item with the given global index. Items are
// laid out 100 per board, in order, so globalIndex 0..99 are board 0 and so on.
export function makeSeedItem(globalIndex: number): { id: string; board: number; item: Item } {
  const board = Math.floor(globalIndex / ITEMS_PER_BOARD);
  const id = `id-${globalIndex}`;
  const subscription = subscriptionFor(board);
  return {
    id,
    board,
    item: {
      id,
      entity: "BACKLOG",
      comboKey: comboKeyFor(subscription, board, id),
      name: `Item ${globalIndex}`,
      subscription,
      parent: `parent-${globalIndex % 97}`,
      ownerId: `owner-${globalIndex % 61}`,
      status: STATUSES[globalIndex % STATUSES.length],
      version: 1,
      updated: 0,
      payload: PAYLOAD,
    },
  };
}

// makePutItem builds a fresh item for the put scenario; id must be unique.
export function makePutItem(id: string, now: number): Item {
  return {
    id,
    entity: "BACKLOG",
    comboKey: `SUB:sub-put|KBN:board-put|ID:${id}`,
    name: `Put ${id}`,
    subscription: "sub-put",
    parent: "parent-put",
    ownerId: "owner-put",
    status: "open",
    version: 1,
    updated: now,
    payload: PAYLOAD,
  };
}

// tableDefinition is the CreateTable input. All seven GSIs are declared; the
// three single-key indexes sit on parent, ownerId and status, and
// entity-comboKey-name-index projects name via INCLUDE.
export function tableDefinition(tableName: string): Record<string, unknown> {
  return {
    TableName: tableName,
    KeySchema: [{ AttributeName: "id", KeyType: "HASH" }],
    AttributeDefinitions: [
      { AttributeName: "comboKey", AttributeType: "S" },
      { AttributeName: "entity", AttributeType: "S" },
      { AttributeName: "id", AttributeType: "S" },
      { AttributeName: "ownerId", AttributeType: "S" },
      { AttributeName: "parent", AttributeType: "S" },
      { AttributeName: "status", AttributeType: "S" },
      { AttributeName: "subscription", AttributeType: "S" },
    ],
    BillingMode: "PAY_PER_REQUEST",
    GlobalSecondaryIndexes: [
      {
        IndexName: "entity-comboKey-index",
        KeySchema: [
          { AttributeName: "entity", KeyType: "HASH" },
          { AttributeName: "comboKey", KeyType: "RANGE" },
        ],
        Projection: { ProjectionType: "ALL" },
      },
      {
        IndexName: "entity-comboKey-name-index",
        KeySchema: [
          { AttributeName: "entity", KeyType: "HASH" },
          { AttributeName: "comboKey", KeyType: "RANGE" },
        ],
        Projection: { ProjectionType: "INCLUDE", NonKeyAttributes: ["name"] },
      },
      {
        IndexName: "comboKey-index",
        KeySchema: [{ AttributeName: "comboKey", KeyType: "HASH" }],
        Projection: { ProjectionType: "ALL" },
      },
      {
        IndexName: "subscription-entity-index",
        KeySchema: [
          { AttributeName: "subscription", KeyType: "HASH" },
          { AttributeName: "entity", KeyType: "RANGE" },
        ],
        Projection: { ProjectionType: "ALL" },
      },
      {
        IndexName: "parent-index",
        KeySchema: [{ AttributeName: "parent", KeyType: "HASH" }],
        Projection: { ProjectionType: "ALL" },
      },
      {
        IndexName: "ownerId-index",
        KeySchema: [{ AttributeName: "ownerId", KeyType: "HASH" }],
        Projection: { ProjectionType: "ALL" },
      },
      {
        IndexName: "status-index",
        KeySchema: [{ AttributeName: "status", KeyType: "HASH" }],
        Projection: { ProjectionType: "ALL" },
      },
    ],
  };
}
