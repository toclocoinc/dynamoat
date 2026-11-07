// DynamoDB Streams: a table with NEW_AND_OLD_IMAGES writes one INSERT, one
// MODIFY and one REMOVE record, read back in order with TRIM_HORIZON.

import {
  DescribeStreamCommand,
  GetRecordsCommand,
  GetShardIteratorCommand,
  ListStreamsCommand,
} from '@aws-sdk/client-dynamodb-streams';
import { DeleteCommand, PutCommand, UpdateCommand } from '@aws-sdk/lib-dynamodb';
import { eq } from '../assert.ts';
import { simpleTable } from './shared.ts';
import type { Case } from './shared.ts';
import type { Ctx } from '../../src/harness.ts';

// collectEventNames reads every shard of the stream with plain sends (not
// captured) until the INSERT, MODIFY and REMOVE records written above have all
// arrived. Real DynamoDB starts a new table's stream with several shards and
// records for one key land in one of them; dynamoat has a single shard.
async function collectEventNames(ctx: Ctx, streamArn: string): Promise<string[]> {
  const deadline = Date.now() + (ctx.target.kind === 'aws' ? 120000 : 30000);
  for (;;) {
    const described = (await ctx.streams.send(new DescribeStreamCommand({ StreamArn: streamArn }))) as {
      StreamDescription?: { Shards?: { ShardId?: string }[] };
    };
    const events: string[] = [];
    for (const shard of described.StreamDescription?.Shards ?? []) {
      const it = (await ctx.streams.send(
        new GetShardIteratorCommand({ StreamArn: streamArn, ShardId: shard.ShardId as string, ShardIteratorType: 'TRIM_HORIZON' }),
      )) as { ShardIterator?: string };
      let iter = it.ShardIterator;
      for (let i = 0; iter && i < 5; i++) {
        const res = (await ctx.streams.send(new GetRecordsCommand({ ShardIterator: iter }))) as {
          Records?: { eventName?: string }[];
          NextShardIterator?: string;
        };
        for (const r of res.Records ?? []) if (r.eventName) events.push(r.eventName);
        if ((res.Records ?? []).length === 0) break;
        iter = res.NextShardIterator;
      }
    }
    if (events.join(',') === 'INSERT,MODIFY,REMOVE') return events;
    if (Date.now() > deadline) throw new Error(`stream records did not arrive in time (saw ${events.join(',') || 'none'})`);
    await new Promise((r) => setTimeout(r, ctx.target.kind === 'aws' ? 2000 : 250));
  }
}

export const streamsCases: Case[] = [
  {
    name: 'streams-new-old-images',
    requires: [],
    slow: true,
    fn: async (ctx) => {
      const table = ctx.table('streams');
      await ctx.createTable({
        ...simpleTable(table),
        StreamSpecification: { StreamEnabled: true, StreamViewType: 'NEW_AND_OLD_IMAGES' },
      });

      await ctx.call(() => ctx.doc.send(new PutCommand({ TableName: table, Item: { id: 's1', qty: 'one' } })));
      await ctx.call(() =>
        ctx.doc.send(
          new UpdateCommand({
            TableName: table,
            Key: { id: 's1' },
            UpdateExpression: 'SET qty = :v',
            ExpressionAttributeValues: { ':v': 'two' },
          }),
        ),
      );
      await ctx.call(() => ctx.doc.send(new DeleteCommand({ TableName: table, Key: { id: 's1' } })));

      const listed = (await ctx.call(() => ctx.streams.send(new ListStreamsCommand({ TableName: table })))) as {
        Streams?: { StreamArn?: string }[];
      };
      const streamArn = listed.Streams?.[0]?.StreamArn;
      eq('streams-new-old-images', typeof streamArn, 'string', 'the stream is listed');

      // Shard layout is implementation-defined (DynamoDB starts with several
      // shards, dynamoat with one), so capture only the stream's defined fields.
      const described = (await ctx.streams.send(new DescribeStreamCommand({ StreamArn: streamArn as string }))) as {
        StreamDescription?: { StreamStatus?: string; StreamViewType?: string; TableName?: string; KeySchema?: unknown; Shards?: unknown[] };
      };
      const sd = described.StreamDescription ?? {};
      eq('streams-new-old-images', (sd.Shards ?? []).length > 0, true, 'the stream has a shard');
      ctx.capture('DescribeStream', {
        StreamStatus: sd.StreamStatus,
        StreamViewType: sd.StreamViewType,
        TableName: sd.TableName,
        KeySchema: ((sd.KeySchema ?? []) as { AttributeName?: string; KeyType?: string }[]).map((k) => ({
          AttributeName: k.AttributeName,
          KeyType: k.KeyType,
        })),
      });

      const got = await collectEventNames(ctx, streamArn as string);
      eq('streams-new-old-images', got.join(','), 'INSERT,MODIFY,REMOVE', 'records arrive in write order');
      ctx.capture('stream-records', { Records: got });
    },
  },
];
