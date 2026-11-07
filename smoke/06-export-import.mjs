// 06-export-import: export a 2,000-item table by parallel Scan to NDJSON and
// DynamoDB JSON files, import into new tables and compare item by item.

import assert from 'node:assert/strict';
import { mkdtemp, rm, readFile, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  CreateTableCommand,
  BatchWriteItemCommand,
  ScanCommand,
} from '@aws-sdk/client-dynamodb';
import { BatchWriteCommand, ScanCommand as DocScanCommand } from '@aws-sdk/lib-dynamodb';
import { run, check, table, send, waitActive } from './lib.mjs';

const key = (pk) => [{ AttributeName: pk, KeyType: 'HASH' }];
const def = (pk, type = 'S') => [{ AttributeName: pk, AttributeType: type }];

await run('06-export-import', async (ctx) => {
  const source = table('export-source');
  await send(ctx.ddb, new CreateTableCommand({
    TableName: source,
    KeySchema: key('pk'),
    AttributeDefinitions: def('pk'),
    BillingMode: 'PAY_PER_REQUEST',
  }));
  await waitActive(ctx.ddb, source);

  // 2,000 items, written with the low-level client in batches of 25.
  const writes = [];
  for (let i = 0; i < 2000; i++) {
    writes.push({ PutRequest: { Item: { pk: { S: `item-${String(i).padStart(4, '0')}` }, n: { N: String(i) }, payload: { S: `payload-${i}` } } } });
  }
  for (let i = 0; i < writes.length; i += 25) {
    await send(ctx.ddb, new BatchWriteItemCommand({ RequestItems: { [source]: writes.slice(i, i + 25) } }));
  }

  async function scanLow(tableName, segments) {
    const all = [];
    for (let s = 0; s < segments; s++) {
      let start;
      do {
        const res = await send(ctx.ddb, new ScanCommand({ TableName: tableName, Segment: s, TotalSegments: segments, ...(start ? { ExclusiveStartKey: start } : {}) }));
        all.push(...(res.Items ?? []));
        start = res.LastEvaluatedKey;
      } while (start);
    }
    return all;
  }

  async function scanDoc(tableName, segments) {
    const all = [];
    for (let s = 0; s < segments; s++) {
      let start;
      do {
        const res = await send(ctx.doc, new DocScanCommand({ TableName: tableName, Segment: s, TotalSegments: segments, ...(start ? { ExclusiveStartKey: start } : {}) }));
        all.push(...(res.Items ?? []));
        start = res.LastEvaluatedKey;
      } while (start);
    }
    return all;
  }

  const byPk = (items, read) => [...items].sort((a, b) => read(a).localeCompare(read(b)));

  await check('export a 2,000-item table by parallel Scan to NDJSON and DynamoDB JSON files', async () => {
    const tmp = await mkdtemp(join(tmpdir(), 'dynamoat-smoke-export-'));
    try {
      const ddbItems = await scanLow(source, 4);
      const nativeItems = await scanDoc(source, 4);
      assert.equal(ddbItems.length, 2000, 'parallel low-level Scan covers every item');
      assert.equal(nativeItems.length, 2000, 'parallel DocumentClient Scan covers every item');

      await writeFile(join(tmp, 'items.ddb.json'), JSON.stringify(ddbItems));
      await writeFile(join(tmp, 'items.ndjson'), nativeItems.map((i) => JSON.stringify(i)).join('\n') + '\n');

      const ddbJson = JSON.parse(await readFile(join(tmp, 'items.ddb.json'), 'utf8'));
      const ndjson = (await readFile(join(tmp, 'items.ndjson'), 'utf8')).trim().split('\n');
      assert.equal(ddbJson.length, 2000);
      assert.equal(ndjson.length, 2000);

      // Parallel Scan returns items in no guaranteed order: sort before checking
      // the boundaries so the assertion is deterministic.
      ddbJson.sort((a, b) => a.pk.S.localeCompare(b.pk.S));
      ndjson.sort((a, b) => JSON.parse(a).pk.localeCompare(JSON.parse(b).pk));
      assert.equal(ddbJson[0].pk.S, 'item-0000');
      assert.deepEqual(JSON.parse(ndjson[0]).pk, 'item-0000');
      assert.equal(ddbJson[1999].pk.S, 'item-1999');
    } finally {
      await rm(tmp, { recursive: true, force: true }).catch(() => {});
    }
  });

  await check('import the DynamoDB JSON into a new table and compare item by item', async () => {
    const tmp = await mkdtemp(join(tmpdir(), 'dynamoat-smoke-export-'));
    try {
      await writeFile(join(tmp, 'items.ddb.json'), JSON.stringify(await scanLow(source, 4)));

      const target = table('import-ddb');
      await send(ctx.ddb, new CreateTableCommand({
        TableName: target,
        KeySchema: key('pk'),
        AttributeDefinitions: def('pk'),
        BillingMode: 'PAY_PER_REQUEST',
      }));
      await waitActive(ctx.ddb, target);

      const items = JSON.parse(await readFile(join(tmp, 'items.ddb.json'), 'utf8'));
      const put = items.map((it) => ({ PutRequest: { Item: it } }));
      for (let i = 0; i < put.length; i += 25) {
        await send(ctx.ddb, new BatchWriteItemCommand({ RequestItems: { [target]: put.slice(i, i + 25) } }));
      }

      const a = byPk(await scanLow(source, 1), (it) => it.pk.S);
      const b = byPk(await scanLow(target, 1), (it) => it.pk.S);
      assert.deepEqual(b, a, 'imported items match the source item by item');
    } finally {
      await rm(tmp, { recursive: true, force: true }).catch(() => {});
    }
  });

  await check('import the NDJSON into a new table and compare item by item', async () => {
    const tmp = await mkdtemp(join(tmpdir(), 'dynamoat-smoke-export-'));
    try {
      const nativeItems = await scanDoc(source, 4);
      await writeFile(join(tmp, 'items.ndjson'), nativeItems.map((i) => JSON.stringify(i)).join('\n') + '\n');

      const target = table('import-ndjson');
      await send(ctx.ddb, new CreateTableCommand({
        TableName: target,
        KeySchema: key('pk'),
        AttributeDefinitions: def('pk'),
        BillingMode: 'PAY_PER_REQUEST',
      }));
      await waitActive(ctx.ddb, target);

      const items = (await readFile(join(tmp, 'items.ndjson'), 'utf8')).trim().split('\n').map((l) => JSON.parse(l));
      const put = items.map((it) => ({ PutRequest: { Item: it } }));
      for (let i = 0; i < put.length; i += 25) {
        await send(ctx.doc, new BatchWriteCommand({ RequestItems: { [target]: put.slice(i, i + 25) } }));
      }

      const a = byPk(await scanDoc(source, 1), (it) => it.pk);
      const b = byPk(await scanDoc(target, 1), (it) => it.pk);
      assert.deepEqual(b, a, 'imported items match the source item by item');
    } finally {
      await rm(tmp, { recursive: true, force: true }).catch(() => {});
    }
  });
});
