// Card edit: SET the name and updated timestamp, ADD one to version, REMOVE
// the assignee, and return the whole item with ALL_NEW.

import { PutCommand, UpdateCommand } from '@aws-sdk/lib-dynamodb';
import { appTenantTable } from './shared.ts';
import type { Case } from '../cases/shared.ts';
import { eq } from '../assert.ts';

const NAME = 'app-card-edit';

export const cardEdit: Case = {
  name: NAME,
  requires: ['UpdateItem'],
  fn: async (ctx) => {
    const table = ctx.table('app-cardedit');
    await ctx.createTable(appTenantTable(table));

    await ctx.call(() =>
      ctx.doc.send(
        new PutCommand({
          TableName: table,
          Item: {
            id: 'card-1',
            entity: 'BACKLOG',
            comboKey: 'SUB:sub-1|KBN:b1|ID:card-1',
            name: 'old name',
            updated: '2026-11-06T11:00:00.000Z',
            version: 1,
            assignee: 'alice',
          },
        }),
      ),
    );

    const updated = (await ctx.call(() =>
      ctx.doc.send(
        new UpdateCommand({
          TableName: table,
          Key: { id: 'card-1' },
          UpdateExpression: 'SET #name = :name, updated = :now ADD version :one REMOVE assignee',
          ExpressionAttributeNames: { '#name': 'name' },
          ExpressionAttributeValues: {
            ':name': 'new name',
            ':now': '2026-11-06T12:00:00.000Z',
            ':one': 1,
          },
          ReturnValues: 'ALL_NEW',
        }),
      ),
    )) as { Attributes?: { name?: string; updated?: string; version?: number; assignee?: string } };

    eq(NAME, updated.Attributes?.name, 'new name', 'ALL_NEW name');
    eq(NAME, updated.Attributes?.updated, '2026-11-06T12:00:00.000Z', 'ALL_NEW updated');
    eq(NAME, updated.Attributes?.version, 2, 'version incremented by 1');
    eq(NAME, Object.hasOwn(updated.Attributes ?? {}, 'assignee'), false, 'assignee removed');
  },
};
