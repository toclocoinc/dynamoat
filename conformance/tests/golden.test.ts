import assert from 'node:assert/strict';
import { test } from 'node:test';
import { diff } from '../src/golden.ts';

test('diff is empty for equal values', () => {
  assert.deepEqual(diff({ a: 1, b: [2, 3] }, { a: 1, b: [2, 3] }), []);
});

test('diff reports readable field differences with paths', () => {
  const lines = diff({ a: 1, b: { c: 'x' } }, { a: 2, b: { c: 'y' } });
  assert.equal(lines.length, 2);
  assert.equal(lines[0], '$.a: expected 1, got 2');
  assert.equal(lines[1], '$.b.c: expected "x", got "y"');
});

test('diff reports missing and extra keys', () => {
  assert.deepEqual(diff({ a: 1 }, { b: 2 }), [
    '$.a: expected 1, got <absent>',
    '$.b: expected <absent>, got 2',
  ]);
});

test('diff reports array length differences', () => {
  assert.deepEqual(diff([1], [1, 2]), ['$[1]: expected <absent>, got 2']);
});
