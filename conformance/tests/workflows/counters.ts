// Counters: ten concurrent UpdateItem calls each add one to a counter with
// if_not_exists, so the final value is 10 regardless of ordering.

import { GetCommand, UpdateCommand } from '@aws-sdk/lib-dynamodb';
import { simpleTable } from '../cases/shared.ts';
import type { Case } from '../cases/shared.ts';
import { eq } from '../assert.ts';

const INCREMENTS = 10;
const NAME = 'app-counters';

export const counters: Case = {
  name: NAME,
  requires: ['UpdateItem'],
  fn: async (ctx) => {
    const table = ctx.table('app-counters');
    await ctx.createTable(simpleTable(table));

    const bump = () =>
      ctx.doc.send(
        new UpdateCommand({
          TableName: table,
          Key: { id: 'counter-1' },
          UpdateExpression: 'SET #n = if_not_exists(#n, :zero) + :one',
          ExpressionAttributeNames: { '#n': 'count' },
          ExpressionAttributeValues: { ':zero': 0, ':one': 1 },
        }),
      );

    await Promise.all(
      Array.from({ length: INCREMENTS }, () => ctx.call(bump)),
    );

    const got = (await ctx.call(() =>
      ctx.doc.send(new GetCommand({ TableName: table, Key: { id: 'counter-1' } })),
    )) as { Item?: { count?: number } };
    eq(NAME, got.Item?.count, INCREMENTS, 'final count');
  },
};
