import test from "node:test";
import assert from "node:assert/strict";
import { promises as fs } from "node:fs";
import os from "node:os";
import path from "node:path";
import { emptyState, loadState, StateWriter, tableState } from "../src/state";

async function tmpdir(): Promise<string> {
  return fs.mkdtemp(path.join(os.tmpdir(), "dynamoat-import-"));
}

test("loadState returns an empty state when the file does not exist", async () => {
  const dir = await tmpdir();
  const state = await loadState(path.join(dir, "missing.json"));
  assert.deepEqual(state, emptyState());
});

test("loadState rejects a file that is not a state file", async () => {
  const dir = await tmpdir();
  const file = path.join(dir, "state.json");
  await fs.writeFile(file, '{"not":"a state file"}');
  await assert.rejects(loadState(file), /not a dynamoat-import state file/);
});

test("StateWriter saves atomically and round-trips", async () => {
  const dir = await tmpdir();
  const file = path.join(dir, "state.json");
  const writer = new StateWriter(file);
  const state = emptyState();
  tableState(state, "t").segments["0"] = { pk: { S: "x" } };
  await writer.save(state);

  assert.deepEqual(await loadState(file), state);
  const leftovers = await fs.readdir(dir);
  assert.ok(!leftovers.some((f) => f.includes(".tmp")), "no temp files left behind");
});
