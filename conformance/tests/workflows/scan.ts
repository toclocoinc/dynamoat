// Scan: a full-table Scan filtered on entity, paged. An application uses this to
// sweep a tenant's cards; the filter is applied by DynamoDB while scanning.

import { PutCommand, ScanCommand } from '@aws-sdk/lib-dynamodb';
import { captureScan } from '../../src/harness.ts';
import { appTenantTable } from './shared.ts';
import type { Case } from '../cases/shared.ts';
import { eq, setEq } from '../assert.ts';

const NAME = 'app-scan-entity-filter';

export const scan: Case = {
  name: NAME,
  requires: ['Scan'],
  fn: async (ctx) => {
    const table = ctx.table('app-scan');
    await ctx.createTable(appTenantTable(table));

    const entities = ['BACKLOG', 'ORDER', 'BACKLOG', 'USER', 'BACKLOG', 'ORDER'];
    for (let i = 0; i < entities.length; i++) {
      await ctx.call(() =>
        ctx.doc.send(
          new PutCommand({
            TableName: table,
            Item: {
              id: `s${i + 1}`,
              entity: entities[i],
              comboKey: `SUB:sub-1|KBN:b1|ID:s${i + 1}`,
              name: `scan ${i + 1}`,
            },
          }),
        ),
      );
    }

    // Scan the whole table, keeping only BACKLOG cards, paged with Limit 2.
    const seen = await captureScan(ctx, 'app-scan-entity-filter-items', async (last) => {
      const params: Record<string, unknown> = {
        TableName: table,
        FilterExpression: 'entity = :e',
        ExpressionAttributeValues: { ':e': 'BACKLOG' },
        Limit: 2,
      };
      if (last) params.ExclusiveStartKey = last;
      const res = (await ctx.doc.send(new ScanCommand(params as never))) as {
        Items?: { id: string; entity: string }[];
        LastEvaluatedKey?: unknown;
      };
      return { items: res.Items ?? [], last: res.LastEvaluatedKey };
    });
    const items = seen.items as { id: string; entity: string }[];

    eq(NAME, items.length, 3, 'BACKLOG items counted once');
    setEq(NAME, items.map((i) => i.id), ['s1', 's3', 's5'], 'BACKLOG items across pages');
    for (const item of items) {
      eq(NAME, item.entity, 'BACKLOG', `entity filter for ${item.id}`);
    }
  },
};
