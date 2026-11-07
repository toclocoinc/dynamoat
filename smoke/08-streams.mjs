// 08-streams: ListStreams, DescribeStream, shard iterators and record images.

import assert from 'node:assert/strict';
import {
  CreateTableCommand,
  PutItemCommand,
  UpdateItemCommand,
  DeleteItemCommand,
} from '@aws-sdk/client-dynamodb';
import {
  ListStreamsCommand,
  DescribeStreamCommand,
  GetShardIteratorCommand,
  GetRecordsCommand,
} from '@aws-sdk/client-dynamodb-streams';
import { run, check, table, send, waitActive, sleep } from './lib.mjs';

const key = (pk) => [{ AttributeName: pk, KeyType: 'HASH' }];
const def = (pk, type = 'S') => [{ AttributeName: pk, AttributeType: type }];

await run('08-streams', async (ctx) => {
  const t = table('streams');
  await send(ctx.ddb, new CreateTableCommand({
    TableName: t,
    KeySchema: key('pk'),
    AttributeDefinitions: def('pk'),
    BillingMode: 'PAY_PER_REQUEST',
    StreamSpecification: { StreamEnabled: true, StreamViewType: 'NEW_AND_OLD_IMAGES' },
  }));
  await waitActive(ctx.ddb, t);

  const pk = (s) => ({ pk: { S: s } });

  let streamArn;
  await check('ListStreams finds the table stream', async () => {
    const res = await send(ctx.streams, new ListStreamsCommand({ TableName: t }));
    const mine = (res.Streams ?? []).filter((s) => s.TableName === t);
    assert.equal(mine.length, 1);
    assert.match(mine[0].StreamArn, /\/stream\//);
    streamArn = mine[0].StreamArn;
  });

  let shardId;
  await check('DescribeStream reports the stream', async () => {
    const res = await send(ctx.streams, new DescribeStreamCommand({ StreamArn: streamArn }));
    const desc = res.StreamDescription;
    assert.equal(desc.StreamViewType, 'NEW_AND_OLD_IMAGES');
    assert.equal(desc.TableName, t);
    assert.equal(desc.StreamStatus, 'ENABLED');
    assert.ok(desc.Shards.length >= 1);
    shardId = desc.Shards[0].ShardId;
  });

  let horizon;
  await check('GetShardIterator TRIM_HORIZON and LATEST', async () => {
    horizon = (await send(ctx.streams, new GetShardIteratorCommand({ StreamArn: streamArn, ShardId: shardId, ShardIteratorType: 'TRIM_HORIZON' }))).ShardIterator;
    assert.ok(horizon.length > 0);
    const latest = (await send(ctx.streams, new GetShardIteratorCommand({ StreamArn: streamArn, ShardId: shardId, ShardIteratorType: 'LATEST' }))).ShardIterator;
    assert.ok(latest.length > 0);
  });

  await check('GetRecords shows INSERT, MODIFY, REMOVE in order with images', async () => {
    await send(ctx.ddb, new PutItemCommand({ TableName: t, Item: { ...pk('a'), val: { S: 'v1' } } }));
    await send(ctx.ddb, new UpdateItemCommand({ TableName: t, Key: pk('a'), UpdateExpression: 'SET val = :v', ExpressionAttributeValues: { ':v': { S: 'v2' } } }));
    await send(ctx.ddb, new DeleteItemCommand({ TableName: t, Key: pk('a') }));

    const records = [];
    let it = horizon;
    const deadline = Date.now() + 30000;
    while (records.length < 3 && Date.now() < deadline) {
      const res = await send(ctx.streams, new GetRecordsCommand({ ShardIterator: it }));
      records.push(...(res.Records ?? []));
      it = res.NextShardIterator;
      if (records.length < 3) await sleep(500);
    }
    assert.equal(records.length, 3);

    const [ins, mod, rem] = records;
    assert.equal(ins.eventName, 'INSERT');
    assert.ok(ins.dynamodb.NewImage);
    assert.equal(ins.dynamodb.OldImage, undefined);
    assert.equal(ins.dynamodb.Keys.pk.S, 'a');

    assert.equal(mod.eventName, 'MODIFY');
    assert.ok(mod.dynamodb.NewImage);
    assert.equal(mod.dynamodb.NewImage.val.S, 'v2');
    assert.ok(mod.dynamodb.OldImage);
    assert.equal(mod.dynamodb.OldImage.val.S, 'v1');

    assert.equal(rem.eventName, 'REMOVE');
    assert.ok(rem.dynamodb.OldImage);
    assert.equal(rem.dynamodb.NewImage, undefined);
  });
});
