import assert from "node:assert/strict";
import { test } from "node:test";
import {
  ITEMS_PER_BOARD,
  boardCount,
  comboKeyFor,
  comboKeyPrefix,
  makePutItem,
  makeSeedItem,
  subscriptionFor,
  tableDefinition,
} from "../src/items.ts";

test("seeded items are BACKLOG-like with 100 items per board", () => {
  const n = 1000;
  const counts = new Map<number, number>();
  for (let i = 0; i < n; i++) {
    const { id, board, item } = makeSeedItem(i);
    assert.equal(item.id, id);
    assert.equal(item.entity, "BACKLOG");
    assert.equal(board, Math.floor(i / ITEMS_PER_BOARD));
    assert.equal(item.subscription, subscriptionFor(board));
    assert.equal(item.comboKey, comboKeyFor(item.subscription as string, board, id));
    counts.set(board, (counts.get(board) ?? 0) + 1);
  }
  assert.equal(boardCount(n), Math.ceil(n / ITEMS_PER_BOARD));
  const fullBoards = Math.floor(n / ITEMS_PER_BOARD);
  for (const [board, count] of counts) {
    if (board < fullBoards) assert.equal(count, ITEMS_PER_BOARD);
  }
});

test("comboKey has the SUB/KBN/ID shape and its prefix selects one board", () => {
  const { item, board } = makeSeedItem(345);
  const subscription = item.subscription as string;
  const comboKey = item.comboKey as string;
  assert.match(comboKey, /^SUB:sub-\d+\|KBN:board-\d+\|ID:id-\d+$/);

  const prefix = comboKeyPrefix(subscription, board);
  assert.ok(comboKey.startsWith(prefix));
  assert.ok(!(makeSeedItem(999).item.comboKey as string).startsWith(prefix));
});

test("items are between 1 and 2 KB", () => {
  for (const i of [0, 1, 99, 100, 9999]) {
    const size = JSON.stringify(makeSeedItem(i).item).length;
    assert.ok(size >= 1000 && size <= 2000, `item ${i} is ${size} bytes`);
  }
});

test("put items are fresh and the table declares all seven GSIs", () => {
  const put = makePutItem("put-7", 123456789);
  assert.equal(put.id, "put-7");
  assert.equal(put.updated, 123456789);
  assert.equal(put.entity, "BACKLOG");

  const def = tableDefinition("bench-x") as {
    KeySchema: { AttributeName: string }[];
    AttributeDefinitions: { AttributeName: string }[];
    GlobalSecondaryIndexes: {
      IndexName: string;
      Projection: { ProjectionType: string; NonKeyAttributes?: string[] };
    }[];
  };

  assert.equal(def.KeySchema[0].AttributeName, "id");
  assert.deepEqual(
    def.GlobalSecondaryIndexes.map((g) => g.IndexName),
    [
      "entity-comboKey-index",
      "entity-comboKey-name-index",
      "comboKey-index",
      "subscription-entity-index",
      "parent-index",
      "ownerId-index",
      "status-index",
    ],
  );

  const defs = new Set(def.AttributeDefinitions.map((d) => d.AttributeName));
  assert.equal(def.AttributeDefinitions.length, 7);
  for (const name of ["id", "entity", "comboKey", "subscription", "parent", "ownerId", "status"]) {
    assert.ok(defs.has(name), `missing AttributeDefinition ${name}`);
  }

  const nameIndex = def.GlobalSecondaryIndexes.find((g) => g.IndexName === "entity-comboKey-name-index")!;
  assert.equal(nameIndex.Projection.ProjectionType, "INCLUDE");
  assert.deepEqual(nameIndex.Projection.NonKeyAttributes, ["name"]);
});
