// 05-bulk: BatchWriteItem / BatchGetItem with retries, a query-then-delete
// workflow, and a whole-table copy.

import assert from 'node:assert/strict';
import {
  CreateTableCommand,
  BatchWriteItemCommand,
  BatchGetItemCommand,
  QueryCommand,
  ScanCommand,
} from '@aws-sdk/client-dynamodb';
import { run, check, table, send, waitActive } from './lib.mjs';

const key = (pk) => [{ AttributeName: pk, KeyType: 'HASH' }];
const def = (pk, type = 'S') => [{ AttributeName: pk, AttributeType: type }];

await run('05-bulk', async (ctx) => {
  const t = table('bulk');
  await send(ctx.ddb, new CreateTableCommand({
    TableName: t,
    KeySchema: key('pk'),
    AttributeDefinitions: def('pk'),
    BillingMode: 'PAY_PER_REQUEST',
  }));
  await waitActive(ctx.ddb, t);

  async function scanAll(tableName) {
    const items = [];
    let start;
    do {
      const res = await send(ctx.ddb, new ScanCommand({ TableName: tableName, ...(start ? { ExclusiveStartKey: start } : {}) }));
      items.push(...(res.Items ?? []));
      start = res.LastEvaluatedKey;
    } while (start);
    return items;
  }

  await check('BatchWriteItem of 1,000 items in batches of 25 with an UnprocessedItems retry loop', async () => {
    let pending = [];
    for (let i = 0; i < 1000; i++) {
      pending.push({ PutRequest: { Item: { pk: { S: `key-${i}` }, val: { N: String(i) } } } });
    }
    let unprocessed = 0;
    for (let round = 0; pending.length > 0 && round < 1000; round++) {
      const res = await send(ctx.ddb, new BatchWriteItemCommand({ RequestItems: { [t]: pending.slice(0, 25) } }));
      const again = res.UnprocessedItems?.[t] ?? [];
      unprocessed += again.length;
      pending = [...again, ...pending.slice(25)];
    }
    assert.equal(pending.length, 0, 'every item is written after retries');
    const items = await scanAll(t);
    assert.equal(items.length, 1000);
  });

  await check('BatchGetItem of 250 keys in batches of 100 with UnprocessedKeys handling', async () => {
    let pending = [];
    for (let i = 0; i < 250; i++) pending.push({ pk: { S: `key-${i}` } });
    const got = new Map();
    for (let round = 0; pending.length > 0 && round < 1000; round++) {
      const res = await send(ctx.ddb, new BatchGetItemCommand({ RequestItems: { [t]: { Keys: pending.slice(0, 100) } } }));
      for (const item of res.Responses?.[t] ?? []) got.set(item.pk.S, item);
      const again = res.UnprocessedKeys?.[t]?.Keys ?? [];
      pending = [...again, ...pending.slice(100)];
    }
    assert.equal(got.size, 250);
    assert.equal(got.get('key-0').val.N, '0');
    assert.equal(got.get('key-249').val.N, '249');
  });

  await check('delete everything a query matches (count, then batch delete)', async () => {
    const del = table('del-query');
    await send(ctx.ddb, new CreateTableCommand({
      TableName: del,
      KeySchema: [{ AttributeName: 'pk', KeyType: 'HASH' }, { AttributeName: 'sk', KeyType: 'RANGE' }],
      AttributeDefinitions: [{ AttributeName: 'pk', AttributeType: 'S' }, { AttributeName: 'sk', AttributeType: 'S' }],
      BillingMode: 'PAY_PER_REQUEST',
    }));
    await waitActive(ctx.ddb, del);

    const writes = [];
    for (let i = 0; i < 50; i++) {
      writes.push({ PutRequest: { Item: { pk: { S: 'group-a' }, sk: { S: `s-${i}` } } } });
      writes.push({ PutRequest: { Item: { pk: { S: 'group-b' }, sk: { S: `s-${i}` } } } });
    }
    for (let i = 0; i < writes.length; i += 25) {
      await send(ctx.ddb, new BatchWriteItemCommand({ RequestItems: { [del]: writes.slice(i, i + 25) } }));
    }

    const counted = await send(ctx.ddb, new QueryCommand({
      TableName: del,
      KeyConditionExpression: 'pk = :p',
      Select: 'COUNT',
      ExpressionAttributeValues: { ':p': { S: 'group-a' } },
    }));
    assert.equal(counted.Count, 50);

    // Collect the keys the query matches, then batch-delete them.
    const keys = [];
    let start;
    do {
      const res = await send(ctx.ddb, new QueryCommand({
        TableName: del,
        KeyConditionExpression: 'pk = :p',
        ExpressionAttributeValues: { ':p': { S: 'group-a' } },
        ...(start ? { ExclusiveStartKey: start } : {}),
      }));
      for (const it of res.Items ?? []) keys.push({ pk: it.pk, sk: it.sk });
      start = res.LastEvaluatedKey;
    } while (start);
    assert.equal(keys.length, 50);

    const deletes = keys.map((k) => ({ DeleteRequest: { Key: k } }));
    for (let i = 0; i < deletes.length; i += 25) {
      await send(ctx.ddb, new BatchWriteItemCommand({ RequestItems: { [del]: deletes.slice(i, i + 25) } }));
    }

    const after = await send(ctx.ddb, new QueryCommand({
      TableName: del,
      KeyConditionExpression: 'pk = :p',
      Select: 'COUNT',
      ExpressionAttributeValues: { ':p': { S: 'group-a' } },
    }));
    assert.equal(after.Count, 0);

    const untouched = await send(ctx.ddb, new QueryCommand({
      TableName: del,
      KeyConditionExpression: 'pk = :p',
      Select: 'COUNT',
      ExpressionAttributeValues: { ':p': { S: 'group-b' } },
    }));
    assert.equal(untouched.Count, 50);
  });

  await check('copy every item from one table to another and compare', async () => {
    const copy = table('copy');
    await send(ctx.ddb, new CreateTableCommand({
      TableName: copy,
      KeySchema: key('pk'),
      AttributeDefinitions: def('pk'),
      BillingMode: 'PAY_PER_REQUEST',
    }));
    await waitActive(ctx.ddb, copy);

    const source = await scanAll(t);
    const put = source.map((it) => ({ PutRequest: { Item: it } }));
    for (let i = 0; i < put.length; i += 25) {
      await send(ctx.ddb, new BatchWriteItemCommand({ RequestItems: { [copy]: put.slice(i, i + 25) } }));
    }

    const dest = await scanAll(copy);
    const byPk = (items) => items.map((i) => i.pk.S).sort();
    assert.deepEqual(byPk(dest), byPk(source));
    const a = [...source].sort((x, y) => x.pk.S.localeCompare(y.pk.S));
    const b = [...dest].sort((x, y) => x.pk.S.localeCompare(y.pk.S));
    assert.deepEqual(b, a, 'copied items match the originals item by item');
  });
});
