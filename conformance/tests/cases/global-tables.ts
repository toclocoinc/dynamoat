// Global tables: add a replica with UpdateTable, see it in DescribeTable,
// remove it. dynamoat treats replicas as bookkeeping (one database serves
// every region), so the replica is ACTIVE immediately after the response.

import { DescribeTableCommand, UpdateTableCommand } from '@aws-sdk/client-dynamodb';
import { simpleTable } from './shared.ts';
import type { Case } from './shared.ts';

export const globalTables: Case[] = [
  {
    name: 'global-tables-replica',
    requires: [],
    dynamoatOnly: true,
    fn: async (ctx) => {
      const table = ctx.table('gt-replica');
      await ctx.createTable(simpleTable(table));
      await ctx.call(() =>
        ctx.ddb.send(
          new UpdateTableCommand({
            TableName: table,
            ReplicaUpdates: [{ Create: { RegionName: 'eu-west-1' } }],
          }),
        ),
      );
      await ctx.call(() => ctx.ddb.send(new DescribeTableCommand({ TableName: table })));
      await ctx.call(() =>
        ctx.ddb.send(
          new UpdateTableCommand({
            TableName: table,
            ReplicaUpdates: [{ Delete: { RegionName: 'eu-west-1' } }],
          }),
        ),
      );
    },
  },
];
