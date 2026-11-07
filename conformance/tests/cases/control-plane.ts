// Control-plane cases: CreateTable, DescribeTable, ListTables.

import { DescribeTableCommand, ListTablesCommand } from '@aws-sdk/client-dynamodb';
import { simpleTable, tenantTable } from './shared.ts';
import type { Case } from './shared.ts';

export const controlPlane: Case[] = [
  {
    name: 'list-tables',
    requires: [],
    fn: async (ctx) => {
      await ctx.createTable(simpleTable(ctx.table('list-alpha')));
      await ctx.createTable(simpleTable(ctx.table('list-beta')));
      await ctx.call(() => ctx.ddb.send(new ListTablesCommand({})));
      await ctx.call(() => ctx.ddb.send(new ListTablesCommand({ Limit: 1 })));
    },
  },
  {
    name: 'create-table',
    requires: [],
    fn: async (ctx) => {
      await ctx.createTable(tenantTable(ctx.table('create-tenant')));
    },
  },
  {
    name: 'describe-table',
    requires: [],
    fn: async (ctx) => {
      await ctx.createTable(tenantTable(ctx.table('describe-tenant')));
      await ctx.call(() => ctx.ddb.send(new DescribeTableCommand({ TableName: ctx.table('describe-tenant') })));
    },
  },
];
