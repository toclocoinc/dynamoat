// 02-table-admin: create, describe, update, TTL, streams and delete tables.

import assert from 'node:assert/strict';
import {
  CreateTableCommand,
  DescribeTableCommand,
  DeleteTableCommand,
  UpdateTableCommand,
  UpdateTimeToLiveCommand,
  DescribeTimeToLiveCommand,
  BatchWriteItemCommand,
  QueryCommand,
} from '@aws-sdk/client-dynamodb';
import { run, check, table, send, waitActive, waitIndexActive, waitGone } from './lib.mjs';

const key = (pk) => [{ AttributeName: pk, KeyType: 'HASH' }];
const def = (pk, type = 'S') => [{ AttributeName: pk, AttributeType: type }];

await run('02-table-admin', async (ctx) => {
  const t1 = table('hash-only');

  await check('CreateTable hash-only PAY_PER_REQUEST, wait ACTIVE', async () => {
    await send(ctx.ddb, new CreateTableCommand({
      TableName: t1,
      KeySchema: key('pk'),
      AttributeDefinitions: def('pk'),
      BillingMode: 'PAY_PER_REQUEST',
    }));
    const t = await waitActive(ctx.ddb, t1);
    assert.equal(t.TableStatus, 'ACTIVE');
    assert.deepEqual(t.KeySchema, [{ AttributeName: 'pk', KeyType: 'HASH' }]);
    assert.equal(t.BillingModeSummary?.BillingMode, 'PAY_PER_REQUEST');
  });

  const t2 = table('hash-range');
  await check('CreateTable hash+range PROVISIONED, wait ACTIVE', async () => {
    await send(ctx.ddb, new CreateTableCommand({
      TableName: t2,
      KeySchema: [{ AttributeName: 'pk', KeyType: 'HASH' }, { AttributeName: 'sk', KeyType: 'RANGE' }],
      AttributeDefinitions: [{ AttributeName: 'pk', AttributeType: 'S' }, { AttributeName: 'sk', AttributeType: 'S' }],
      ProvisionedThroughput: { ReadCapacityUnits: 5, WriteCapacityUnits: 5 },
    }));
    const t = await waitActive(ctx.ddb, t2);
    assert.equal(t.TableStatus, 'ACTIVE');
    assert.deepEqual(t.KeySchema, [
      { AttributeName: 'pk', KeyType: 'HASH' },
      { AttributeName: 'sk', KeyType: 'RANGE' },
    ]);
    assert.equal(t.ProvisionedThroughput.ReadCapacityUnits, 5);
    assert.equal(t.ProvisionedThroughput.WriteCapacityUnits, 5);
  });

  const t3 = table('with-gsis');
  await check('CreateTable with GSIs (ALL, KEYS_ONLY, INCLUDE)', async () => {
    await send(ctx.ddb, new CreateTableCommand({
      TableName: t3,
      KeySchema: key('pk'),
      AttributeDefinitions: [
        { AttributeName: 'pk', AttributeType: 'S' },
        { AttributeName: 'g1', AttributeType: 'S' },
        { AttributeName: 'g2', AttributeType: 'S' },
        { AttributeName: 'g3', AttributeType: 'S' },
      ],
      BillingMode: 'PAY_PER_REQUEST',
      GlobalSecondaryIndexes: [
        { IndexName: 'gsi-all', KeySchema: key('g1'), Projection: { ProjectionType: 'ALL' } },
        { IndexName: 'gsi-keys', KeySchema: key('g2'), Projection: { ProjectionType: 'KEYS_ONLY' } },
        { IndexName: 'gsi-include', KeySchema: key('g3'), Projection: { ProjectionType: 'INCLUDE', NonKeyAttributes: ['extra'] } },
      ],
    }));
    const t = await waitActive(ctx.ddb, t3);
    const byName = Object.fromEntries(t.GlobalSecondaryIndexes.map((g) => [g.IndexName, g]));
    assert.equal(byName['gsi-all'].Projection.ProjectionType, 'ALL');
    assert.equal(byName['gsi-keys'].Projection.ProjectionType, 'KEYS_ONLY');
    assert.equal(byName['gsi-include'].Projection.ProjectionType, 'INCLUDE');
    assert.deepEqual(byName['gsi-include'].Projection.NonKeyAttributes, ['extra']);
  });

  const t4 = table('add-gsi');
  await check('UpdateTable adding a GSI to a table with 500 items, wait until queryable', async () => {
    await send(ctx.ddb, new CreateTableCommand({
      TableName: t4,
      KeySchema: key('pk'),
      AttributeDefinitions: def('pk'),
      BillingMode: 'PAY_PER_REQUEST',
    }));
    await waitActive(ctx.ddb, t4);

    // 500 items across 5 GSI partitions.
    const writes = [];
    for (let i = 0; i < 500; i++) {
      writes.push({ PutRequest: { Item: { pk: { S: `item-${i}` }, 'gsi-pk': { S: `cat-${i % 5}` }, val: { N: String(i) } } } });
    }
    for (let i = 0; i < writes.length; i += 25) {
      await send(ctx.ddb, new BatchWriteItemCommand({ RequestItems: { [t4]: writes.slice(i, i + 25) } }));
    }

    await send(ctx.ddb, new UpdateTableCommand({
      TableName: t4,
      AttributeDefinitions: [{ AttributeName: 'gsi-pk', AttributeType: 'S' }],
      GlobalSecondaryIndexUpdates: [{
        Create: {
          IndexName: 'gsi-late',
          KeySchema: key('gsi-pk'),
          Projection: { ProjectionType: 'ALL' },
        },
      }],
    }));
    await waitIndexActive(ctx.ddb, t4, 'gsi-late');

    const res = await send(ctx.ddb, new QueryCommand({
      TableName: t4,
      IndexName: 'gsi-late',
      KeyConditionExpression: '#g = :c',
      ExpressionAttributeNames: { '#g': 'gsi-pk' },
      ExpressionAttributeValues: { ':c': { S: 'cat-0' } },
    }));
    assert.equal(res.Count, 100, 'the new index is backfilled and queryable');
    assert.ok(res.Items.every((it) => it['gsi-pk'].S === 'cat-0'));
  });

  await check('delete a GSI', async () => {
    await send(ctx.ddb, new UpdateTableCommand({
      TableName: t4,
      GlobalSecondaryIndexUpdates: [{ Delete: { IndexName: 'gsi-late' } }],
    }));
    const desc = await send(ctx.ddb, new DescribeTableCommand({ TableName: t4 }));
    assert.equal((desc.Table.GlobalSecondaryIndexes ?? []).some((g) => g.IndexName === 'gsi-late'), false);
  });

  await check('switch billing mode', async () => {
    await send(ctx.ddb, new UpdateTableCommand({
      TableName: t4,
      BillingMode: 'PROVISIONED',
      ProvisionedThroughput: { ReadCapacityUnits: 2, WriteCapacityUnits: 2 },
    }));
    const provisioned = await send(ctx.ddb, new DescribeTableCommand({ TableName: t4 }));
    assert.equal(provisioned.Table.ProvisionedThroughput.ReadCapacityUnits, 2);
    assert.equal(provisioned.Table.BillingModeSummary, undefined);

    await send(ctx.ddb, new UpdateTableCommand({ TableName: t4, BillingMode: 'PAY_PER_REQUEST' }));
    const ppr = await send(ctx.ddb, new DescribeTableCommand({ TableName: t4 }));
    assert.equal(ppr.Table.BillingModeSummary.BillingMode, 'PAY_PER_REQUEST');
  });

  await check('UpdateTimeToLive + DescribeTimeToLive', async () => {
    const enabled = await send(ctx.ddb, new UpdateTimeToLiveCommand({
      TableName: t1,
      TimeToLiveSpecification: { Enabled: true, AttributeName: 'ttl' },
    }));
    assert.deepEqual(enabled.TimeToLiveSpecification, { Enabled: true, AttributeName: 'ttl' });

    const desc = await send(ctx.ddb, new DescribeTimeToLiveCommand({ TableName: t1 }));
    assert.equal(desc.TimeToLiveDescription.TimeToLiveStatus, 'ENABLED');
    assert.equal(desc.TimeToLiveDescription.AttributeName, 'ttl');

    await send(ctx.ddb, new UpdateTimeToLiveCommand({
      TableName: t1,
      TimeToLiveSpecification: { Enabled: false, AttributeName: 'ttl' },
    }));
    const off = await send(ctx.ddb, new DescribeTimeToLiveCommand({ TableName: t1 }));
    assert.equal(off.TimeToLiveDescription.TimeToLiveStatus, 'DISABLED');
  });

  await check('enable a stream with UpdateTable and read LatestStreamArn', async () => {
    const res = await send(ctx.ddb, new UpdateTableCommand({
      TableName: t1,
      StreamSpecification: { StreamEnabled: true, StreamViewType: 'NEW_AND_OLD_IMAGES' },
    }));
    const spec = res.TableDescription.StreamSpecification;
    assert.equal(spec.StreamEnabled, true);
    assert.equal(spec.StreamViewType, 'NEW_AND_OLD_IMAGES');
    assert.match(res.TableDescription.LatestStreamArn, /\/stream\//);
  });

  await check('DeleteTable and wait until gone', async () => {
    await send(ctx.ddb, new DeleteTableCommand({ TableName: t1 }));
    await waitGone(ctx.ddb, t1);
  });
});
