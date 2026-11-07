// 04-query-scan: key conditions, filters, projections, paging, ordering,
// COUNT, parallel Scan, GSI queries and consumed capacity.

import assert from 'node:assert/strict';
import {
  CreateTableCommand,
  BatchWriteItemCommand,
  QueryCommand,
  ScanCommand,
} from '@aws-sdk/client-dynamodb';
import { run, check, table, send, waitActive } from './lib.mjs';

await run('04-query-scan', async (ctx) => {
  const t = table('query-scan');
  await send(ctx.ddb, new CreateTableCommand({
    TableName: t,
    KeySchema: [{ AttributeName: 'pk', KeyType: 'HASH' }, { AttributeName: 'sk', KeyType: 'RANGE' }],
    AttributeDefinitions: [
      { AttributeName: 'pk', AttributeType: 'S' },
      { AttributeName: 'sk', AttributeType: 'S' },
      { AttributeName: 'grp', AttributeType: 'S' },
    ],
    BillingMode: 'PAY_PER_REQUEST',
    GlobalSecondaryIndexes: [{
      IndexName: 'gsi-grp',
      KeySchema: [{ AttributeName: 'grp', KeyType: 'HASH' }],
      Projection: { ProjectionType: 'ALL' },
    }],
  }));
  await waitActive(ctx.ddb, t);

  // 300 items over 3 partitions, 100 each, with a GSI partition and assorted
  // attributes for the filter tests.
  const rows = [];
  for (let i = 0; i < 300; i++) {
    rows.push({
      pk: `p${Math.floor(i / 100)}`,
      sk: `sk-${String(i % 100).padStart(3, '0')}`,
      n: i,
      label: `label-${i}`,
      grp: `grp-${i % 3}`,
      flag: i % 2 === 0,
      maybe: i % 2 === 0,
      nested: { x: `x-${i}`, y: i },
    });
  }
  const write = [];
  for (const r of rows) {
    const item = {
      pk: { S: r.pk },
      sk: { S: r.sk },
      n: { N: String(r.n) },
      label: { S: r.label },
      grp: { S: r.grp },
      flag: { BOOL: r.flag },
      nested: { M: { x: { S: r.nested.x }, y: { N: String(r.nested.y) } } },
    };
    if (r.maybe) item.maybe = { S: 'yes' };
    write.push({ PutRequest: { Item: item } });
  }
  for (let i = 0; i < write.length; i += 25) {
    await send(ctx.ddb, new BatchWriteItemCommand({ RequestItems: { [t]: write.slice(i, i + 25) } }));
  }

  const keysOf = (items) => items.map((i) => `${i.pk.S}|${i.sk.S}`).sort();
  const skOf = (items) => items.map((i) => i.sk.S);

  async function scanAll(params) {
    const items = [];
    let start;
    do {
      const res = await send(ctx.ddb, new ScanCommand({ TableName: t, ...params, ...(start ? { ExclusiveStartKey: start } : {}) }));
      items.push(...(res.Items ?? []));
      start = res.LastEvaluatedKey;
    } while (start);
    return items;
  }

  async function queryAll(params) {
    const items = [];
    let start;
    do {
      const res = await send(ctx.ddb, new QueryCommand({ TableName: t, ...params, ...(start ? { ExclusiveStartKey: start } : {}) }));
      items.push(...(res.Items ?? []));
      start = res.LastEvaluatedKey;
    } while (start);
    return items;
  }

  await check('every key condition operator', async () => {
    const p = { ':p': { S: 'p0' } };

    const eq = await queryAll({ KeyConditionExpression: 'pk = :p AND sk = :s', ExpressionAttributeValues: { ...p, ':s': { S: 'sk-050' } } });
    assert.deepEqual(skOf(eq), ['sk-050']);

    const lt = await queryAll({ KeyConditionExpression: 'pk = :p AND sk < :s', ExpressionAttributeValues: { ...p, ':s': { S: 'sk-003' } } });
    assert.deepEqual(skOf(lt), ['sk-000', 'sk-001', 'sk-002']);

    const le = await queryAll({ KeyConditionExpression: 'pk = :p AND sk <= :s', ExpressionAttributeValues: { ...p, ':s': { S: 'sk-002' } } });
    assert.deepEqual(skOf(le), ['sk-000', 'sk-001', 'sk-002']);

    const gt = await queryAll({ KeyConditionExpression: 'pk = :p AND sk > :s', ExpressionAttributeValues: { ...p, ':s': { S: 'sk-096' } } });
    assert.deepEqual(skOf(gt), ['sk-097', 'sk-098', 'sk-099']);

    const ge = await queryAll({ KeyConditionExpression: 'pk = :p AND sk >= :s', ExpressionAttributeValues: { ...p, ':s': { S: 'sk-097' } } });
    assert.deepEqual(skOf(ge), ['sk-097', 'sk-098', 'sk-099']);

    const between = await queryAll({ KeyConditionExpression: 'pk = :p AND sk BETWEEN :lo AND :hi', ExpressionAttributeValues: { ...p, ':lo': { S: 'sk-010' }, ':hi': { S: 'sk-012' } } });
    assert.deepEqual(skOf(between), ['sk-010', 'sk-011', 'sk-012']);

    const begins = await queryAll({ KeyConditionExpression: 'pk = :p AND begins_with(sk, :pre)', ExpressionAttributeValues: { ...p, ':pre': { S: 'sk-00' } } });
    assert.equal(begins.length, 10);
    assert.deepEqual(skOf(begins)[0], 'sk-000');
    assert.deepEqual(skOf(begins)[9], 'sk-009');
  });

  await check('FilterExpression with AND/OR/NOT, begins_with, contains, attribute_exists, size', async () => {
    const expect = (pred) => rows.filter(pred).map((r) => `${r.pk}|${r.sk}`).sort();

    const and = await scanAll({ FilterExpression: 'grp = :g AND n >= :n', ExpressionAttributeValues: { ':g': { S: 'grp-1' }, ':n': { N: '150' } } });
    assert.deepEqual(keysOf(and), expect((r) => r.grp === 'grp-1' && r.n >= 150));

    const or = await scanAll({ FilterExpression: 'grp = :g0 OR grp = :g1', ExpressionAttributeValues: { ':g0': { S: 'grp-0' }, ':g1': { S: 'grp-2' } } });
    assert.deepEqual(keysOf(or), expect((r) => r.grp === 'grp-0' || r.grp === 'grp-2'));

    const not = await scanAll({ FilterExpression: 'NOT grp = :g', ExpressionAttributeValues: { ':g': { S: 'grp-0' } } });
    assert.deepEqual(keysOf(not), expect((r) => r.grp !== 'grp-0'));

    const begins = await scanAll({ FilterExpression: 'begins_with(label, :pre)', ExpressionAttributeValues: { ':pre': { S: 'label-2' } } });
    assert.deepEqual(keysOf(begins), expect((r) => r.label.startsWith('label-2')));

    const contains = await scanAll({ FilterExpression: 'contains(label, :sub)', ExpressionAttributeValues: { ':sub': { S: '42' } } });
    assert.deepEqual(keysOf(contains), expect((r) => r.label.includes('42')));

    const exists = await scanAll({ FilterExpression: 'attribute_exists(maybe)' });
    assert.deepEqual(keysOf(exists), expect((r) => r.maybe));

    const sized = await scanAll({ FilterExpression: 'size(label) = :len', ExpressionAttributeValues: { ':len': { N: '8' } } });
    assert.deepEqual(keysOf(sized), expect((r) => r.label.length === 8));
  });

  await check('ProjectionExpression with nested paths', async () => {
    const res = await send(ctx.ddb, new QueryCommand({
      TableName: t,
      KeyConditionExpression: 'pk = :p AND sk = :s',
      ProjectionExpression: 'pk, sk, nested.x',
      ExpressionAttributeValues: { ':p': { S: 'p0' }, ':s': { S: 'sk-007' } },
    }));
    assert.equal(res.Items.length, 1);
    const item = res.Items[0];
    assert.deepEqual(Object.keys(item).sort(), ['nested', 'pk', 'sk']);
    assert.deepEqual(item.nested, { M: { x: { S: 'x-7' } } });
  });

  await check('"load more" paging with Limit and ExclusiveStartKey until done', async () => {
    const collected = [];
    let start;
    do {
      const res = await send(ctx.ddb, new QueryCommand({
        TableName: t,
        KeyConditionExpression: 'pk = :p',
        ExpressionAttributeValues: { ':p': { S: 'p0' } },
        Limit: 40,
        ...(start ? { ExclusiveStartKey: start } : {}),
      }));
      collected.push(...(res.Items ?? []));
      start = res.LastEvaluatedKey;
    } while (start);

    const sks = skOf(collected);
    assert.equal(collected.length, 100);
    assert.equal(new Set(sks).size, 100, 'no duplicates across pages');
    assert.deepEqual(sks, Array.from({ length: 100 }, (_, i) => `sk-${String(i).padStart(3, '0')}`), 'no gaps, ascending');
  });

  await check('ScanIndexForward false', async () => {
    const desc = await queryAll({
      KeyConditionExpression: 'pk = :p',
      ExpressionAttributeValues: { ':p': { S: 'p0' } },
      ScanIndexForward: false,
    });
    assert.deepEqual(skOf(desc), Array.from({ length: 100 }, (_, i) => `sk-${String(99 - i).padStart(3, '0')}`));
  });

  await check('Select COUNT', async () => {
    const res = await send(ctx.ddb, new QueryCommand({
      TableName: t,
      KeyConditionExpression: 'pk = :p',
      ExpressionAttributeValues: { ':p': { S: 'p0' } },
      Select: 'COUNT',
    }));
    assert.equal(res.Count, 100);
    assert.equal(res.Items, undefined);
  });

  await check('parallel Scan with 4 segments whose union is every item once', async () => {
    const union = [];
    for (let segment = 0; segment < 4; segment++) {
      const items = await scanAll({ Segment: segment, TotalSegments: 4 });
      union.push(...items);
    }
    assert.equal(union.length, 300);
    assert.equal(new Set(keysOf(union)).size, 300);
  });

  await check('Query on a GSI', async () => {
    const items = await queryAll({
      IndexName: 'gsi-grp',
      KeyConditionExpression: 'grp = :g',
      ExpressionAttributeValues: { ':g': { S: 'grp-1' } },
    });
    assert.equal(items.length, 100);
    assert.ok(items.every((i) => i.grp.S === 'grp-1'));
  });

  await check('ReturnConsumedCapacity TOTAL present', async () => {
    const res = await send(ctx.ddb, new ScanCommand({ TableName: t, Limit: 1, ReturnConsumedCapacity: 'TOTAL' }));
    assert.ok(res.ConsumedCapacity, 'ConsumedCapacity is present');
    assert.equal(typeof res.ConsumedCapacity.CapacityUnits, 'number');
    assert.equal(res.ConsumedCapacity.TableName, t);
  });
});
