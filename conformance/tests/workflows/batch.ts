// Batch: BatchGet of 60 ids paged with at most 100 keys per request and a
// projection, and BatchWrite of 50 items in chunks of 25.

import { BatchGetCommand, BatchWriteCommand } from '@aws-sdk/lib-dynamodb';
import { chunk } from './shared.ts';
import { simpleTable } from '../cases/shared.ts';
import type { Case } from '../cases/shared.ts';
import { deep, eq, setEq } from '../assert.ts';

const NAME = 'app-batch';

export const batch: Case = {
  name: NAME,
  requires: ['BatchGetItem', 'BatchWriteItem'],
  fn: async (ctx) => {
    const table = ctx.table('app-batch');
    await ctx.createTable(simpleTable(table));

    // Seed 60 items for the BatchGet, writing them in BatchWrite chunks of 25.
    const seedIds = Array.from({ length: 60 }, (_, i) => `item${i + 1}`);
    for (const ids of chunk(seedIds, 25)) {
      await ctx.call(() =>
        ctx.doc.send(
          new BatchWriteCommand({
            RequestItems: {
              [table]: ids.map((id) => ({ PutRequest: { Item: { id, value: `value-${id}` } } })),
            },
          }),
        ),
      );
    }

    // BatchGet the 60 ids in pages of at most 100 keys, with a projection.
    const got: { id: string; value: string }[] = [];
    for (const ids of chunk(seedIds, 100)) {
      const res = (await ctx.call(() =>
        ctx.doc.send(
          new BatchGetCommand({
            RequestItems: {
              [table]: {
                Keys: ids.map((id) => ({ id })),
                ProjectionExpression: 'id, #v',
                ExpressionAttributeNames: { '#v': 'value' },
              },
            },
          }),
        ),
      )) as { Responses?: Record<string, { id: string; value: string }[]> };
      got.push(...(res.Responses?.[table] ?? []));
    }
    eq(NAME, got.length, seedIds.length, 'BatchGet returns every requested id once');
    setEq(NAME, got.map((item) => item.id), seedIds, 'BatchGet requested ids');
    for (const item of got) {
      deep(NAME, Object.keys(item).sort(), ['id', 'value'], `projected attributes for ${item.id}`);
    }

    // BatchWrite 50 items in chunks of 25.
    const writeIds = Array.from({ length: 50 }, (_, i) => `extra${i + 1}`);
    for (const ids of chunk(writeIds, 25)) {
      await ctx.call(() =>
        ctx.doc.send(
          new BatchWriteCommand({
            RequestItems: {
              [table]: ids.map((id) => ({ PutRequest: { Item: { id, value: `extra-value-${id}` } } })),
            },
          }),
        ),
      );
    }

    // Confirm all 50 exist (read directly, so the extra request is not recorded).
    const check = (await ctx.doc.send(
      new BatchGetCommand({
        RequestItems: { [table]: { Keys: writeIds.map((id) => ({ id })) } },
      }),
    )) as { Responses?: Record<string, { id: string }[]> };
    eq(NAME, (check.Responses?.[table] ?? []).length, writeIds.length, 'all 50 BatchWrite items exist');
  },
};
