import test from "node:test";
import assert from "node:assert/strict";
import { canonicalJson, itemChecksum, totalChecksum } from "../src/checksum";
import type { Item } from "../src/types";

const itemA: Item = {
  pk: { S: "a" },
  n: { N: "1.5" },
  nested: { M: { z: { S: "1" }, a: { N: "2" } } },
};

const itemB: Item = {
  pk: { S: "b" },
  n: { N: "2" },
  list: { L: [{ N: "3" }, { S: "x" }] },
};

test("canonicalJson sorts object keys at every level", () => {
  assert.equal(canonicalJson({ b: { S: "1" }, a: { N: "2" } }), '{"a":{"N":"2"},"b":{"S":"1"}}');
  assert.equal(canonicalJson({ m: { M: { z: { N: "1" }, a: { N: "2" } } } }), '{"m":{"M":{"a":{"N":"2"},"z":{"N":"1"}}}}');
});

test("numbers are kept as their DynamoDB strings", () => {
  assert.notEqual(itemChecksum({ n: { N: "1.5" } }), itemChecksum({ n: { N: "1.50" } }));
});

test("the same items in another order give the same sum", () => {
  assert.equal(totalChecksum([itemA, itemB]), totalChecksum([itemB, itemA]));
});

test("one changed attribute changes the checksum", () => {
  const changed: Item = JSON.parse(JSON.stringify(itemA)) as Item;
  changed.n = { N: "1.6" };
  assert.notEqual(itemChecksum(itemA), itemChecksum(changed));
});

test("list order is part of an item's checksum", () => {
  const x: Item = { l: { L: [{ N: "1" }, { N: "2" }] } };
  const y: Item = { l: { L: [{ N: "2" }, { N: "1" }] } };
  assert.notEqual(itemChecksum(x), itemChecksum(y));
});
