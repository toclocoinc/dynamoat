// Error cases for operations dynamoat already implements.

import { GetItemCommand } from '@aws-sdk/client-dynamodb';
import { GetCommand, PutCommand } from '@aws-sdk/lib-dynamodb';
import { simpleTable } from './shared.ts';
import type { Case } from './shared.ts';

export const errors: Case[] = [
  {
    name: 'error-missing-table',
    requires: [],
    fn: async (ctx) => {
      await ctx.call(() =>
        ctx.ddb.send(new GetItemCommand({ TableName: ctx.table('missing-table'), Key: { id: { S: 'x' } } })),
      );
    },
  },
  {
    name: 'error-wrong-key-type',
    requires: [],
    fn: async (ctx) => {
      const table = ctx.table('wrongkey');
      await ctx.createTable(simpleTable(table));
      // The table key "id" is S, but the request provides an N.
      await ctx.call(() => ctx.ddb.send(new GetItemCommand({ TableName: table, Key: { id: { N: '5' } } })));
    },
  },
  {
    name: 'error-unused-eav',
    requires: [],
    fn: async (ctx) => {
      const table = ctx.table('unused-eav');
      await ctx.createTable(simpleTable(table));
      await ctx.call(() =>
        ctx.doc.send(
          new PutCommand({
            TableName: table,
            Item: { id: 'u1' },
            ConditionExpression: 'attribute_not_exists(id)',
            ExpressionAttributeValues: { ':unused': 'x' },
          }),
        ),
      );
    },
  },
  {
    name: 'error-reserved-word',
    requires: [],
    fn: async (ctx) => {
      const table = ctx.table('reserved-word');
      await ctx.createTable(simpleTable(table));
      // "name" is a reserved word, used here as a bare attribute name.
      await ctx.call(() => ctx.doc.send(new GetCommand({ TableName: table, Key: { id: 'r1' }, ProjectionExpression: 'name' })));
    },
  },
  {
    name: 'error-item-too-large',
    requires: [],
    fn: async (ctx) => {
      const table = ctx.table('big-item');
      await ctx.createTable(simpleTable(table));
      await ctx.call(() =>
        ctx.doc.send(
          new PutCommand({ TableName: table, Item: { id: 'big', payload: 'x'.repeat(400 * 1024 + 1) } }),
        ),
      );
    },
  },
];
