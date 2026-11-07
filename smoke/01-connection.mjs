// 01-connection: reach the endpoint, page through ListTables, and confirm a
// missing table is a ResourceNotFoundException.

import assert from 'node:assert/strict';
import {
  CreateTableCommand,
  DescribeTableCommand,
  ListTablesCommand,
} from '@aws-sdk/client-dynamodb';
import { run, check, table, send, Unsupported } from './lib.mjs';

await run('01-connection', async (ctx) => {
  await check('GET /healthz', async () => {
    if (ctx.aws) throw new Unsupported('healthz (dynamoat only)');
    const res = await fetch(`${ctx.endpoint}/healthz`);
    assert.equal(res.status, 200);
    assert.match(await res.text(), /healthy/);
  });

  await check('GET /readyz', async () => {
    if (ctx.aws) throw new Unsupported('readyz (dynamoat only)');
    const res = await fetch(`${ctx.endpoint}/readyz`);
    assert.equal(res.status, 200);
    assert.match(await res.text(), /ready/);
  });

  await check('ListTables paging with Limit and ExclusiveStartTableName over 7 tables', async () => {
    const names = [];
    for (let i = 0; i < 7; i++) names.push(table(`page-${String.fromCharCode(97 + i)}`));
    for (const n of names) {
      await send(ctx.ddb, new CreateTableCommand({
        TableName: n,
        KeySchema: [{ AttributeName: 'pk', KeyType: 'HASH' }],
        AttributeDefinitions: [{ AttributeName: 'pk', AttributeType: 'S' }],
        BillingMode: 'PAY_PER_REQUEST',
      }));
    }

    const seen = [];
    let last;
    let pages = 0;
    for (;;) {
      pages++;
      const res = await send(ctx.ddb, new ListTablesCommand({
        Limit: 3,
        ...(last ? { ExclusiveStartTableName: last } : {}),
      }));
      const page = res.TableNames ?? [];
      assert.ok(page.length <= 3, `page ${pages} respects Limit`);
      if (page.length === 3) {
        assert.equal(res.LastEvaluatedTableName, page[page.length - 1], `page ${pages} carries a consistent LastEvaluatedTableName`);
      }
      seen.push(...page);
      if (!res.LastEvaluatedTableName) break;
      last = res.LastEvaluatedTableName;
    }
    assert.equal(new Set(seen).size, seen.length, 'no table name appears twice across pages');

    const ours = seen.filter((n) => n.startsWith(`smoke-${ctx.prefix}-page-`));
    assert.deepEqual(ours, [...names].sort(), 'our seven tables are all found, in byte order');
  });

  await check('DescribeTable of a missing table', async () => {
    const missing = `smoke-${ctx.prefix}-does-not-exist`;
    let name;
    try {
      await send(ctx.ddb, new DescribeTableCommand({ TableName: missing }));
    } catch (e) {
      name = e.name;
    }
    assert.equal(name, 'ResourceNotFoundException');
  });
});
