// Conditional create: a Put guarded by attribute_not_exists(id). The first
// put inserts, the second fails because the id now exists.

import { PutCommand } from '@aws-sdk/lib-dynamodb';
import { simpleTable } from '../cases/shared.ts';
import type { Case } from '../cases/shared.ts';
import { deep, eq } from '../assert.ts';

const NAME = 'app-conditional-create';

export const conditionalCreate: Case = {
  name: NAME,
  requires: [],
  fn: async (ctx) => {
    const table = ctx.table('app-condcreate');
    await ctx.createTable(simpleTable(table));

    const first = await ctx.call(() =>
      ctx.doc.send(
        new PutCommand({
          TableName: table,
          Item: { id: 'c1', value: 'first' },
          ConditionExpression: 'attribute_not_exists(id)',
        }),
      ),
    );
    eq(NAME, (first as { $metadata?: { httpStatusCode?: number } }).$metadata?.httpStatusCode, 200, 'first Put succeeds');

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
    eq(NAME, (second as { name?: string }).name, 'ConditionalCheckFailedException', 'second Put fails');
    deep(NAME, (second as { Item?: unknown }).Item, { id: { S: 'c1' }, value: { S: 'first' } }, 'item remains the first one');
  },
};
