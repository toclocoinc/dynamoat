// An application creates one tenant table per subscription: hash key id,
// OLD_IMAGE stream, and the four GSIs the listing/scanning Lambdas query.

import { DescribeTableCommand } from '@aws-sdk/client-dynamodb';
import { appTenantTable } from './shared.ts';
import type { Case } from '../cases/shared.ts';

export const tenantTable: Case = {
  name: 'app-tenant-table',
  requires: [],
  fn: async (ctx) => {
    const table = ctx.table('app-tenant');
    await ctx.createTable(appTenantTable(table));
    // The application waits for ACTIVE before touching a new tenant table.
    await ctx.call(() => ctx.ddb.send(new DescribeTableCommand({ TableName: table })));
  },
};
