// PartiQL operations: ExecuteStatement, BatchExecuteStatement and
// ExecuteTransaction, exercised through the DocumentClient exactly as a
// SQL-editor session would. The statements use literal values (no ?), so the
// SDK sends no Parameters.

import {
  BatchExecuteStatementCommand,
  ExecuteStatementCommand,
  ExecuteTransactionCommand,
} from '@aws-sdk/lib-dynamodb';
import { captureScan } from '../../src/harness.ts';
import { simpleTable } from './shared.ts';
import type { Case } from './shared.ts';
import { deep, eq } from '../assert.ts';

interface StatementResult {
  Items?: Record<string, unknown>[];
  NextToken?: string;
  Responses?: Record<string, unknown>[];
}

export const partiqlCases: Case[] = [
  {
    name: 'partiql-editor-session',
    requires: ['ExecuteStatement', 'BatchExecuteStatement', 'ExecuteTransaction'],
    fn: async (ctx) => {
      const table = ctx.table('partiql');
      const qt = `"${table}"`;
      await ctx.createTable(simpleTable(table));

      // INSERT three items, then SELECT one back by key.
      await ctx.call(() =>
        ctx.doc.send(new ExecuteStatementCommand({ Statement: `INSERT INTO ${qt} VALUE {'id':'a','n':1,'tag':'x'}` })),
      );
      await ctx.call(() =>
        ctx.doc.send(new ExecuteStatementCommand({ Statement: `INSERT INTO ${qt} VALUE {'id':'b','n':2,'tag':'x'}` })),
      );
      await ctx.call(() =>
        ctx.doc.send(new ExecuteStatementCommand({ Statement: `INSERT INTO ${qt} VALUE {'id':'c','n':3,'tag':'y'}` })),
      );

      const byKey = (await ctx.call(() =>
        ctx.doc.send(new ExecuteStatementCommand({ Statement: `SELECT * FROM ${qt} WHERE id = 'a'` })),
      )) as StatementResult;
      eq('partiql-editor-session', byKey.Items?.length, 1, 'select by key returns one item');
      eq('partiql-editor-session', byKey.Items?.[0].id, 'a', 'select by key returns the right id');

      // SELECT with a non-key filter is a Scan with the whole WHERE as a filter.
      const filtered = await captureScan(ctx, 'partiql-filter-select', async (last) => {
        const res = (await ctx.doc.send(
          new ExecuteStatementCommand({
            Statement: `SELECT * FROM ${qt} WHERE tag = 'x'`,
            ...(last === undefined || last === null ? {} : { NextToken: last as string }),
          }),
        )) as StatementResult;
        return { items: res.Items ?? [], last: res.NextToken };
      });
      eq('partiql-editor-session', filtered.items.length, 2, 'filter on a non-key attribute matches two items');

      // Paging with Limit and NextToken visits every item.
      const paged = await captureScan(ctx, 'partiql-full-select', async (last) => {
        const res = (await ctx.doc.send(
          new ExecuteStatementCommand({
            Statement: `SELECT * FROM ${qt}`,
            Limit: 2,
            ...(last === undefined || last === null ? {} : { NextToken: last as string }),
          }),
        )) as StatementResult;
        return { items: res.Items ?? [], last: res.NextToken };
      });
      eq('partiql-editor-session', paged.items.length, 3, 'paging visits all items');

      // UPDATE an item, then read it back.
      await ctx.call(() =>
        ctx.doc.send(new ExecuteStatementCommand({ Statement: `UPDATE ${qt} SET n = 5 WHERE id = 'a'` })),
      );
      const updated = (await ctx.call(() =>
        ctx.doc.send(new ExecuteStatementCommand({ Statement: `SELECT * FROM ${qt} WHERE id = 'a'` })),
      )) as StatementResult;
      eq('partiql-editor-session', updated.Items?.[0].n, 5, 'UPDATE changes the item');

      // BatchExecuteStatement: one read that hits, one that misses.
      const batch = (await ctx.call(() =>
        ctx.doc.send(
          new BatchExecuteStatementCommand({
            Statements: [
              { Statement: `SELECT * FROM ${qt} WHERE id = 'b'` },
              { Statement: `SELECT * FROM ${qt} WHERE id = 'missing'` },
            ],
          }),
        ),
      )) as { Responses?: { TableName?: string; Item?: Record<string, unknown> }[] };
      deep('partiql-editor-session', batch.Responses?.[0].Item, { id: 'b', n: 2, tag: 'x' }, 'batch read present item');
      eq('partiql-editor-session', batch.Responses?.[1].Item, undefined, 'batch read missing item has no Item');

      // ExecuteTransaction: two INSERTs in one transaction.
      await ctx.call(() =>
        ctx.doc.send(
          new ExecuteTransactionCommand({
            TransactStatements: [
              { Statement: `INSERT INTO ${qt} VALUE {'id':'d','n':4}` },
              { Statement: `INSERT INTO ${qt} VALUE {'id':'e','n':5}` },
            ],
          }),
        ),
      );

      // DELETE an item, then confirm it is gone.
      await ctx.call(() =>
        ctx.doc.send(new ExecuteStatementCommand({ Statement: `DELETE FROM ${qt} WHERE id = 'a'` })),
      );
      const afterDelete = (await ctx.call(() =>
        ctx.doc.send(new ExecuteStatementCommand({ Statement: `SELECT * FROM ${qt} WHERE id = 'a'` })),
      )) as StatementResult;
      eq('partiql-editor-session', afterDelete.Items?.length, 0, 'deleted item is gone');
    },
  },
];
