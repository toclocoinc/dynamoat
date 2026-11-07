// 03-items: attribute round trips, targeted updates, conditional writes and
// the 400 KB item-size boundary.

import assert from 'node:assert/strict';
import {
  CreateTableCommand,
  GetItemCommand,
  PutItemCommand,
  UpdateItemCommand,
  DeleteItemCommand,
} from '@aws-sdk/client-dynamodb';
import { GetCommand, PutCommand, NumberValue } from '@aws-sdk/lib-dynamodb';
import { run, check, table, send, waitActive } from './lib.mjs';

const key = (pk) => [{ AttributeName: pk, KeyType: 'HASH' }];
const def = (pk, type = 'S') => [{ AttributeName: pk, AttributeType: type }];

// b64 normalises a binary attribute, which the SDK returns as either a base64
// string or a Uint8Array, to a base64 string for comparison.
function b64(v) {
  if (typeof v === 'string') return v;
  if (v instanceof Uint8Array) return Buffer.from(v).toString('base64');
  throw new Error(`unexpected binary value: ${typeof v}`);
}

function setOf(v) {
  return [...v].sort();
}

await run('03-items', async (ctx) => {
  const t = table('items');
  await send(ctx.ddb, new CreateTableCommand({
    TableName: t,
    KeySchema: [{ AttributeName: 'pk', KeyType: 'HASH' }, { AttributeName: 'sk', KeyType: 'RANGE' }],
    AttributeDefinitions: [{ AttributeName: 'pk', AttributeType: 'S' }, { AttributeName: 'sk', AttributeType: 'S' }],
    BillingMode: 'PAY_PER_REQUEST',
  }));
  await waitActive(ctx.ddb, t);

  await check('round trip all ten attribute types (low-level client)', async () => {
    const b1 = Buffer.from([1, 2, 3]);
    const b2 = Buffer.from([4, 5]);
    const b1b64 = b1.toString('base64');
    const b2b64 = b2.toString('base64');
    const item = {
      pk: { S: 'types' },
      sk: { S: 'low' },
      s: { S: 'hello' },
      n: { N: '12345678901234567890123456789012345678' },
      neg: { N: '-3.14159' },
      dec: { N: '0.000001' },
      b: { B: b1 },
      bool: { BOOL: true },
      nullv: { NULL: true },
      m: { M: { a: { S: 'x' }, b: { N: '2' } } },
      l: { L: [{ S: 'a' }, { N: '2' }, { BOOL: false }] },
      ss: { SS: ['a', 'b'] },
      ns: { NS: ['1', '2'] },
      bs: { BS: [b1, b2] },
    };
    await send(ctx.ddb, new PutItemCommand({ TableName: t, Item: item }));
    const got = (await send(ctx.ddb, new GetItemCommand({ TableName: t, Key: { pk: { S: 'types' }, sk: { S: 'low' } } }))).Item;

    assert.equal(got.s.S, 'hello');
    assert.equal(got.n.N, '12345678901234567890123456789012345678');
    assert.equal(got.neg.N, '-3.14159');
    assert.equal(got.dec.N, '0.000001');
    assert.equal(b64(got.b.B), b1b64);
    assert.equal(got.bool.BOOL, true);
    assert.equal(got.nullv.NULL, true);
    assert.deepEqual(got.m.M, { a: { S: 'x' }, b: { N: '2' } });
    assert.deepEqual(got.l.L, [{ S: 'a' }, { N: '2' }, { BOOL: false }]);
    assert.deepEqual(setOf(got.ss.SS), ['a', 'b']);
    assert.deepEqual(setOf(got.ns.NS), ['1', '2']);
    assert.deepEqual(got.bs.BS.map(b64), [b1b64, b2b64]);
  });

  await check('round trip all ten attribute types (DocumentClient)', async () => {
    const item = {
      pk: 'types',
      sk: 'doc',
      s: 'hello',
      n: 12345,
      big: new NumberValue('12345678901234567890123456789012345678'),
      neg: -3.14159,
      dec: 0.001,
      b: new Uint8Array([1, 2, 3]),
      bool: true,
      nullv: null,
      m: { a: 'x', b: 2 },
      l: ['a', 2, false],
      ss: new Set(['a', 'b']),
      ns: new Set([1, 2]),
      bs: new Set([new Uint8Array([4, 5])]),
    };
    await send(ctx.doc, new PutCommand({ TableName: t, Item: item }));
    const got = (await send(ctx.doc, new GetCommand({ TableName: t, Key: { pk: 'types', sk: 'doc' } }))).Item;

    assert.equal(got.s, 'hello');
    assert.equal(got.n, 12345);
    assert.equal(got.big.toString(), '12345678901234567890123456789012345678');
    assert.equal(got.neg, -3.14159);
    assert.equal(got.dec, 0.001);
    assert.deepEqual(Buffer.from(got.b), Buffer.from([1, 2, 3]));
    assert.equal(got.bool, true);
    assert.equal(got.nullv, null);
    assert.deepEqual(got.m, { a: 'x', b: 2 });
    assert.deepEqual(got.l, ['a', 2, false]);
    assert.deepEqual(setOf(got.ss), ['a', 'b']);
    assert.deepEqual(setOf(got.ns), [1, 2]);
    assert.equal(setOf(got.bs).length, 1);
    assert.deepEqual(Buffer.from([...got.bs][0]), Buffer.from([4, 5]));
  });

  await check('targeted UpdateItem (SET nested path, REMOVE, ADD, list_append)', async () => {
    await send(ctx.ddb, new PutItemCommand({
      TableName: t,
      Item: {
        pk: { S: 'u1' },
        sk: { S: 's1' },
        qty: { N: '1' },
        nums: { NS: ['1'] },
        tags: { L: [{ S: 'a' }] },
        info: { M: { name: { S: 'old' }, stale: { S: 'del' } } },
      },
    }));

    await send(ctx.ddb, new UpdateItemCommand({
      TableName: t,
      Key: { pk: { S: 'u1' }, sk: { S: 's1' } },
      UpdateExpression: 'SET info.#n = :name, tags = list_append(tags, :more) REMOVE info.stale ADD qty :one, nums :n',
      ExpressionAttributeNames: { '#n': 'name' },
      ExpressionAttributeValues: {
        ':name': { S: 'new' },
        ':more': { L: [{ S: 'b' }, { S: 'c' }] },
        ':one': { N: '1' },
        ':n': { NS: ['2'] },
      },
    }));

    const got = (await send(ctx.ddb, new GetItemCommand({ TableName: t, Key: { pk: { S: 'u1' }, sk: { S: 's1' } } }))).Item;
    assert.deepEqual(got.info, { M: { name: { S: 'new' } } });
    assert.deepEqual(got.tags, { L: [{ S: 'a' }, { S: 'b' }, { S: 'c' }] });
    assert.equal(got.qty.N, '2');
    assert.deepEqual(setOf(got.nums.NS), ['1', '2']);
  });

  await check('optimistic concurrency (stale write fails with ALL_OLD)', async () => {
    const key = { pk: { S: 'c1' }, sk: { S: 's1' } };
    await send(ctx.ddb, new PutItemCommand({ TableName: t, Item: { ...key, version: { N: '1' }, val: { S: 'v1' } } }));

    await send(ctx.ddb, new UpdateItemCommand({
      TableName: t,
      Key: key,
      ConditionExpression: 'version = :one',
      UpdateExpression: 'SET val = :v2, version = version + :one',
      ExpressionAttributeValues: { ':one': { N: '1' }, ':v2': { S: 'v2' } },
    }));

    let error;
    try {
      await send(ctx.ddb, new UpdateItemCommand({
        TableName: t,
        Key: key,
        ConditionExpression: 'version = :one',
        UpdateExpression: 'SET val = :v3',
        ReturnValuesOnConditionCheckFailure: 'ALL_OLD',
        ExpressionAttributeValues: { ':one': { N: '1' }, ':v3': { S: 'v3' } },
      }));
    } catch (e) {
      error = e;
    }
    assert.equal(error?.name, 'ConditionalCheckFailedException');
    assert.equal(error.Item.version.N, '2', 'ALL_OLD carries the current item');
    assert.equal(error.Item.val.S, 'v2');
  });

  await check('delete with a condition', async () => {
    const key = { pk: { S: 'd1' }, sk: { S: 's1' } };
    await send(ctx.ddb, new PutItemCommand({ TableName: t, Item: { ...key, version: { N: '1' } } }));
    await send(ctx.ddb, new DeleteItemCommand({
      TableName: t,
      Key: key,
      ConditionExpression: 'version = :one',
      ExpressionAttributeValues: { ':one': { N: '1' } },
    }));
    const got = (await send(ctx.ddb, new GetItemCommand({ TableName: t, Key: key }))).Item;
    assert.equal(got, undefined);
  });

  await check('item just under 400 KB succeeds and just over fails', async () => {
    const sizeTable = table('size');
    await send(ctx.ddb, new CreateTableCommand({
      TableName: sizeTable,
      KeySchema: key('pk'),
      AttributeDefinitions: def('pk'),
      BillingMode: 'PAY_PER_REQUEST',
    }));
    await waitActive(ctx.ddb, sizeTable);

    // Item size is 2 (name "pk") + 1 (value) + 1 (name "p") + payload length.
    const under = 'a'.repeat(409595); // 409599 bytes: just under 400 KB
    const over = 'a'.repeat(409597); // 409601 bytes: just over 400 KB
    await send(ctx.ddb, new PutItemCommand({ TableName: sizeTable, Item: { pk: { S: 'u' }, p: { S: under } } }));

    let errorName;
    try {
      await send(ctx.ddb, new PutItemCommand({ TableName: sizeTable, Item: { pk: { S: 'o' }, p: { S: over } } }));
    } catch (e) {
      errorName = e.name;
    }
    assert.equal(errorName, 'ValidationException');
  });
});
