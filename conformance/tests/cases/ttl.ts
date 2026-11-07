// TTL cases: UpdateTimeToLive and DescribeTimeToLive, sent through the
// DocumentClient. The command classes live in client-dynamodb; lib-dynamodb
// re-uses them rather than re-exporting them.

import { DescribeTimeToLiveCommand, UpdateTimeToLiveCommand } from '@aws-sdk/client-dynamodb';
import { simpleTable } from './shared.ts';
import type { Case } from './shared.ts';

export const ttl: Case[] = [
  {
    name: 'time-to-live',
    requires: ['UpdateTimeToLive', 'DescribeTimeToLive'],
    fn: async (ctx) => {
      const table = ctx.table('ttl');
      await ctx.createTable(simpleTable(table));

      await ctx.call(() =>
        ctx.doc.send(
          new UpdateTimeToLiveCommand({
            TableName: table,
            TimeToLiveSpecification: { Enabled: true, AttributeName: 'expiresAt' },
          }),
        ),
      );
      await ctx.call(() => ctx.doc.send(new DescribeTimeToLiveCommand({ TableName: table })));
    },
  },
];
