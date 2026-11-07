// Item operations merged into dynamoat: PutItem, GetItem, DeleteItem.

import { GetItemCommand, PutItemCommand } from '@aws-sdk/client-dynamodb';
import { DeleteCommand, GetCommand, PutCommand } from '@aws-sdk/lib-dynamodb';
import { simpleTable } from './shared.ts';
import type { Case } from './shared.ts';
import { deep, eq, setEq } from '../assert.ts';

export const itemOps: Case[] = [
  {
    name: 'putitem-getitem-roundtrip',
    requires: [],
    fn: async (ctx) => {
      const table = ctx.table('roundtrip');
      await ctx.createTable(simpleTable(table));

      // A 38-digit number must round-trip exactly (plain client keeps N as a
      // string), alongside string and number sets.
      const big = '12345678901234567890123456789012345678';
      await ctx.call(() =>
        ctx.ddb.send(
          new PutItemCommand({
            TableName: table,
            Item: {
              id: { S: 'roundtrip-1' },
              big: { N: big },
              ss: { SS: ['a', 'b', 'c'] },
              ns: { NS: ['1', '2.5', '3'] },
            },
          }),
        ),
      );
      const got = (await ctx.call(() =>
        ctx.ddb.send(new GetItemCommand({ TableName: table, Key: { id: { S: 'roundtrip-1' } } })),
      )) as {
        Item?: {
          id?: { S?: string };
          big?: { N?: string };
          ss?: { SS?: string[] };
          ns?: { NS?: string[] };
        };
      };
      eq('putitem-getitem-roundtrip', got.Item?.id?.S, 'roundtrip-1', 'id round trip');
      eq('putitem-getitem-roundtrip', got.Item?.big?.N, big, '38-digit number round trip');
      setEq('putitem-getitem-roundtrip', got.Item?.ss?.SS ?? [], ['a', 'b', 'c'], 'string set round trip');
      setEq('putitem-getitem-roundtrip', got.Item?.ns?.NS ?? [], ['1', '2.5', '3'], 'number set round trip');
    },
  },
  {
    name: 'putitem-conditional',
    requires: [],
    fn: async (ctx) => {
      const table = ctx.table('conditional');
      await ctx.createTable(simpleTable(table));

      // attribute_not_exists succeeds on the first put and fails on the second;
      // the second asks for the old item back.
      const first = await ctx.call(() =>
        ctx.doc.send(
          new PutCommand({
            TableName: table,
            Item: { id: 'c1', value: 'first' },
            ConditionExpression: 'attribute_not_exists(id)',
          }),
        ),
      );
      eq('putitem-conditional', (first as { $metadata?: { httpStatusCode?: number } }).$metadata?.httpStatusCode, 200, 'first Put succeeds');

      const second = await ctx.call(() =>
        ctx.doc.send(
          new PutCommand({
            TableName: table,
            Item: { id: 'c1', value: 'second' },
            ConditionExpression: 'attribute_not_exists(id)',
            ReturnValuesOnConditionCheckFailure: 'ALL_OLD',
          }),
        ),
      );
      eq('putitem-conditional', (second as { name?: string }).name, 'ConditionalCheckFailedException', 'second Put fails');
      deep('putitem-conditional', (second as { Item?: unknown }).Item, { id: { S: 'c1' }, value: { S: 'first' } }, 'item remains the first one');
    },
  },
  {
    name: 'delete-item',
    requires: [],
    fn: async (ctx) => {
      const table = ctx.table('deleteitem');
      await ctx.createTable(simpleTable(table));

      await ctx.call(() => ctx.doc.send(new PutCommand({ TableName: table, Item: { id: 'd1', value: 'x' } })));
      const deleted = await ctx.call(() =>
        ctx.doc.send(new DeleteCommand({ TableName: table, Key: { id: 'd1' }, ReturnValues: 'ALL_OLD' })),
      );
      deep('delete-item', (deleted as { Attributes?: unknown }).Attributes, { id: 'd1', value: 'x' }, 'ALL_OLD returns the deleted item');
      const got = await ctx.call(() => ctx.doc.send(new GetCommand({ TableName: table, Key: { id: 'd1' } })));
      eq('delete-item', (got as { Item?: unknown }).Item, undefined, 'deleted item is gone');
    },
  },
];
