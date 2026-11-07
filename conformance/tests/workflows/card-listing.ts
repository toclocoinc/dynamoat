// Card listing: 120 BACKLOG cards across 3 boards, queried per board through
// the name-only GSI with begins_with on comboKey, paged with Limit 25.

import { PutCommand, QueryCommand } from '@aws-sdk/lib-dynamodb';
import { appTenantTable } from './shared.ts';
import type { Case } from '../cases/shared.ts';
import { deep, eq, setEq } from '../assert.ts';

const ENTITY = 'BACKLOG';
const SUB = 'sub-1';
const BOARDS = ['b1', 'b2', 'b3'];
const PER_BOARD = 40;
const NAME = 'app-card-listing';

interface Card {
  id: string;
  entity: string;
  comboKey: string;
  name: string;
}

export const cardListing: Case = {
  name: NAME,
  requires: ['Query'],
  fn: async (ctx) => {
    const table = ctx.table('app-cards');
    await ctx.createTable(appTenantTable(table));

    // 120 cards, 40 per board. comboKey is SUB:<s>|KBN:<board>|ID:<id>.
    for (let b = 0; b < BOARDS.length; b++) {
      for (let i = 1; i <= PER_BOARD; i++) {
        const id = `c${String(b * PER_BOARD + i).padStart(3, '0')}`;
        await ctx.call(() =>
          ctx.doc.send(
            new PutCommand({
              TableName: table,
              Item: {
                id,
                entity: ENTITY,
                comboKey: `SUB:${SUB}|KBN:${BOARDS[b]}|ID:${id}`,
                name: `Card ${id}`,
                // Attributes the name-only index does not project.
                status: 'OPEN',
                points: i,
              },
            }),
          ),
        );
      }
    }

    // List board b1 through the INCLUDE [name] index: only the projected
    // attributes (id, entity, comboKey, name) come back, paged with Limit 25.
    const prefix = `SUB:${SUB}|KBN:${BOARDS[0]}|`;
    const seen: Card[] = [];
    let last: unknown;
    for (let page = 0; page < 10; page++) {
      const params: Record<string, unknown> = {
        TableName: table,
        IndexName: 'entity-comboKey-name-index',
        KeyConditionExpression: 'entity = :e AND begins_with(comboKey, :p)',
        ExpressionAttributeValues: { ':e': ENTITY, ':p': prefix },
        // ALL_PROJECTED_ATTRIBUTES is DynamoDB's default Select for an index;
        // it is spelled out so the request is identical everywhere.
        Select: 'ALL_PROJECTED_ATTRIBUTES',
        Limit: 25,
      };
      if (last) params.ExclusiveStartKey = last;
      const res = (await ctx.call(() => ctx.doc.send(new QueryCommand(params as never)))) as {
        Items?: Card[];
        LastEvaluatedKey?: unknown;
      };
      seen.push(...(res.Items ?? []));
      last = res.LastEvaluatedKey;
      if (!last) break;
    }

    const expectedIds = Array.from({ length: PER_BOARD }, (_, i) => `c${String(i + 1).padStart(3, '0')}`);
    eq(NAME, seen.length, PER_BOARD, 'board b1 items visited');
    setEq(NAME, seen.map((i) => i.id), expectedIds, 'board b1 ids visited exactly once');
    deep(NAME, seen.map((i) => i.comboKey), [...seen.map((i) => i.comboKey)].sort(), 'items in ascending comboKey order');
    for (const item of seen) {
      deep(NAME, Object.keys(item).sort(), ['comboKey', 'entity', 'id', 'name'], `projected attributes for ${item.id}`);
    }
    eq(NAME, last, undefined, 'last page has no LastEvaluatedKey');
  },
};
