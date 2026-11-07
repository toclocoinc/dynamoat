// 09-partiql: ExecuteStatement and BatchExecuteStatement workflows. dynamoat
// does not implement PartiQL yet, so against dynamoat these checks report
// unsupported; the calls are written the way real DynamoDB expects them.

import assert from 'node:assert/strict';
import {
  CreateTableCommand,
  GetItemCommand,
  PutItemCommand,
  ExecuteStatementCommand,
  BatchExecuteStatementCommand,
} from '@aws-sdk/client-dynamodb';
import { run, check, table, send, waitActive } from './lib.mjs';

const key = (pk) => [{ AttributeName: pk, KeyType: 'HASH' }];
const def = (pk, type = 'S') => [{ AttributeName: pk, AttributeType: type }];

await run('09-partiql', async (ctx) => {
  const t = table('partiql');
  await send(ctx.ddb, new CreateTableCommand({
    TableName: t,
    KeySchema: key('pk'),
    AttributeDefinitions: def('pk'),
    BillingMode: 'PAY_PER_REQUEST',
  }));
  await waitActive(ctx.ddb, t);

  await send(ctx.ddb, new PutItemCommand({ TableName: t, Item: { pk: { S: 'a' }, val: { N: '1' } } }));
  const quoted = `"${t}"`;

  await check('ExecuteStatement SELECT with a WHERE on the key', async () => {
    const res = await send(ctx.ddb, new ExecuteStatementCommand({ Statement: `SELECT * FROM ${quoted} WHERE "pk" = 'a'` }));
    assert.ok(Array.isArray(res.Items));
    assert.equal(res.Items[0].pk.S, 'a');
  });

  await check('ExecuteStatement SELECT with parameters', async () => {
    const res = await send(ctx.ddb, new ExecuteStatementCommand({
      Statement: `SELECT * FROM ${quoted} WHERE "pk" = ?`,
      Parameters: [{ S: 'a' }],
    }));
    assert.ok(Array.isArray(res.Items));
    assert.equal(res.Items[0].val.N, '1');
  });

  await check('ExecuteStatement INSERT', async () => {
    await send(ctx.ddb, new ExecuteStatementCommand({ Statement: `INSERT INTO ${quoted} VALUE {'pk': 'b', 'val': 2}` }));
    const got = (await send(ctx.ddb, new GetItemCommand({ TableName: t, Key: { pk: { S: 'b' } } }))).Item;
    assert.equal(got.val.N, '2');
  });

  await check('ExecuteStatement UPDATE', async () => {
    await send(ctx.ddb, new ExecuteStatementCommand({ Statement: `UPDATE ${quoted} SET val = 3 WHERE "pk" = 'b'` }));
    const got = (await send(ctx.ddb, new GetItemCommand({ TableName: t, Key: { pk: { S: 'b' } } }))).Item;
    assert.equal(got.val.N, '3');
  });

  await check('ExecuteStatement DELETE', async () => {
    await send(ctx.ddb, new ExecuteStatementCommand({ Statement: `DELETE FROM ${quoted} WHERE "pk" = 'b'` }));
    const got = (await send(ctx.ddb, new GetItemCommand({ TableName: t, Key: { pk: { S: 'b' } } }))).Item;
    assert.equal(got, undefined);
  });

  await check('ExecuteStatement paging with NextToken', async () => {
    const items = [];
    let next;
    do {
      const res = await send(ctx.ddb, new ExecuteStatementCommand({
        Statement: `SELECT * FROM ${quoted}`,
        ...(next ? { NextToken: next } : {}),
      }));
      items.push(...(res.Items ?? []));
      next = res.NextToken;
    } while (next);
    assert.ok(items.length >= 1);
  });

  await check('BatchExecuteStatement', async () => {
    const res = await send(ctx.ddb, new BatchExecuteStatementCommand({
      Statements: [
        { Statement: `SELECT * FROM ${quoted} WHERE "pk" = ?`, Parameters: [{ S: 'a' }] },
        { Statement: `SELECT * FROM ${quoted} WHERE "pk" = ?`, Parameters: [{ S: 'a' }] },
      ],
    }));
    // Each response is {TableName, Item} or {Error}: one item per statement.
    assert.equal(res.Responses.length, 2);
    for (const r of res.Responses) {
      assert.equal(r.Error, undefined, `statement failed: ${JSON.stringify(r.Error)}`);
      assert.equal(r.Item.pk.S, 'a');
    }
  });
});
