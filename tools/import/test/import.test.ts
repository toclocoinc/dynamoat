import test from "node:test";
import assert from "node:assert/strict";
import { promises as fs } from "node:fs";
import os from "node:os";
import path from "node:path";
import { importTables } from "../src/import";
import { loadState } from "../src/state";
import { basicDesc, FakeReader, FakeWriter, keyOf, makeItem } from "./fakes";
import type { Item, TableDescription } from "../src/types";

const noProgress = (): void => {};

function items(n: number): Item[] {
  return Array.from({ length: n }, (_, i) => makeItem(`item-${i}`));
}

test("copies every item when the target table does not exist", async () => {
  const reader = new FakeReader(items(10));
  const writer = new FakeWriter();
  const summary = await importTables({
    reader,
    writer,
    tables: ["t"],
    segments: 1,
    statePath: path.join(await fs.mkdtemp(path.join(os.tmpdir(), "dynamoat-import-")), "state.json"),
    verify: true,
    dryRun: false,
    progress: noProgress,
  });

  assert.equal(writer.createCalls.length, 1);
  assert.equal(writer.written.length, 10);
  assert.equal(summary.ok, true);
  assert.equal(summary.tables[0].status, "created");
  assert.equal(summary.tables[0].verify?.match, true);
  assert.equal(summary.tables[0].verify?.sourceCount, 10);
  assert.equal(summary.tables[0].verify?.targetCount, 10);
});

test("resumes segments from a state file", async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "dynamoat-import-"));
  const statePath = path.join(dir, "state.json");
  // A previous run finished segment 1 and copied segment 0 up to item-4.
  await fs.writeFile(
    statePath,
    JSON.stringify({
      version: 1,
      tables: {
        t: {
          done: false,
          segments: { "0": { pk: { S: "item-4" } }, "1": null },
        },
      },
    }),
  );

  const reader = new FakeReader(items(10));
  const writer = new FakeWriter();
  // The previous run already created the target table; only the copy resumes.
  writer.tables.set("t", basicDesc("t"));
  const summary = await importTables({
    reader,
    writer,
    tables: ["t"],
    segments: 2,
    statePath,
    verify: false,
    dryRun: false,
    progress: noProgress,
  });

  const pks = writer.written.map((it) => it.pk?.S).sort();
  // Segment 0 resumes after item-4 (items 6 and 8); segment 1 is already done.
  assert.deepEqual(pks, ["item-6", "item-8"]);
  assert.equal(summary.tables[0].status, "resumed");

  const state = await loadState(statePath);
  const t = state.tables["t"];
  assert.equal(t.done, true);
  assert.equal(t.segments["0"], null);
  assert.equal(t.segments["1"], null);
});

test("a target table with a different key schema is an error", async () => {
  const reader = new FakeReader(items(3));
  const writer = new FakeWriter();
  const other: TableDescription = {
    TableName: "t",
    TableStatus: "ACTIVE",
    KeySchema: [{ AttributeName: "id", KeyType: "HASH" }],
    AttributeDefinitions: [{ AttributeName: "id", AttributeType: "S" }],
  };
  await writer.createTable(other);

  const summary = await importTables({
    reader,
    writer,
    tables: ["t"],
    segments: 1,
    statePath: path.join(await fs.mkdtemp(path.join(os.tmpdir(), "dynamoat-import-")), "state.json"),
    verify: false,
    dryRun: false,
    progress: noProgress,
  });

  assert.equal(summary.ok, false);
  assert.equal(summary.tables[0].status, "failed");
  assert.match(summary.tables[0].error ?? "", /different key schema/);
  assert.equal(writer.written.length, 0);
});

test("a matching existing key schema resumes and copies items", async () => {
  const reader = new FakeReader(items(5));
  const writer = new FakeWriter();
  writer.tables.set("t", basicDesc("t")); // same key schema as the source

  const summary = await importTables({
    reader,
    writer,
    tables: ["t"],
    segments: 1,
    statePath: path.join(await fs.mkdtemp(path.join(os.tmpdir(), "dynamoat-import-")), "state.json"),
    verify: false,
    dryRun: false,
    progress: noProgress,
  });

  assert.equal(writer.createCalls.length, 0);
  assert.equal(writer.written.length, 5);
  assert.equal(summary.tables[0].status, "resumed");
});

test("dry-run reports the plan without writing or creating", async () => {
  const reader = new FakeReader(items(5));
  const writer = new FakeWriter();
  const summary = await importTables({
    reader,
    writer,
    tables: ["t"],
    segments: 1,
    statePath: path.join(await fs.mkdtemp(path.join(os.tmpdir(), "dynamoat-import-")), "state.json"),
    verify: false,
    dryRun: true,
    progress: noProgress,
  });

  assert.equal(summary.tables[0].status, "dry-run");
  assert.equal(writer.createCalls.length, 0);
  assert.equal(writer.written.length, 0);
});

// keyOf is exercised through the fake's resume path; this keeps the import from
// being flagged as an unused helper in review.
test("keyOf picks the key attributes from the key schema", () => {
  const desc = basicDesc();
  assert.deepEqual(keyOf(makeItem("x"), desc), { pk: { S: "x" } });
});
