// 07-transactions: TransactWriteItems success, cancellation reasons, idempotent
// replay, and TransactGetItems (unsupported on dynamoat today).

import assert from 'node:assert/strict';
import {
  CreateTableCommand,
  GetItemCommand,
  PutItemCommand,
  TransactWriteItemsCommand,
  TransactGetItemsCommand,
} from '@aws-sdk/client-dynamodb';
import { run, check, table, send, waitActive } from './lib.mjs';

const key = (pk) => [{ AttributeName: pk, KeyType: 'HASH' }];
const def = (pk, type = 'S') => [{ AttributeName: pk, AttributeType: type }];

await run('07-transactions', async (ctx) => {
  const t = table('txn');
  await send(ctx.ddb, new CreateTableCommand({
    TableName: t,
    KeySchema: key('pk'),
    AttributeDefinitions: def('pk'),
    BillingMode: 'PAY_PER_REQUEST',
  }));
  await waitActive(ctx.ddb, t);

  const pk = (s) => ({ pk: { S: s } });
  const get = async (s) => (await send(ctx.ddb, new GetItemCommand({ TableName: t, Key: pk(s) }))).Item;

  await send(ctx.ddb, new PutItemCommand({ TableName: t, Item: { ...pk('a'), version: { N: '1' } } }));
  await send(ctx.ddb, new PutItemCommand({ TableName: t, Item: { ...pk('c'), qty: { N: '0' } } }));
  await send(ctx.ddb, new PutItemCommand({ TableName: t, Item: { ...pk('d'), val: { S: 'bye' } } }));
  await send(ctx.ddb, new PutItemCommand({ TableName: t, Item: { ...pk('y'), version: { N: '1' } } }));
  await send(ctx.ddb, new PutItemCommand({ TableName: t, Item: { ...pk('cnt'), qty: { N: '0' } } }));

  await check('TransactWriteItems success (Put, Update, Delete, ConditionCheck)', async () => {
    await send(ctx.ddb, new TransactWriteItemsCommand({
      TransactItems: [
        { ConditionCheck: { TableName: t, Key: pk('a'), ConditionExpression: 'version = :one', ExpressionAttributeValues: { ':one': { N: '1' } } } },
        { Put: { TableName: t, Item: { ...pk('b'), val: { S: 'new' } } } },
        { Update: { TableName: t, Key: pk('c'), UpdateExpression: 'SET qty = qty + :inc', ExpressionAttributeValues: { ':inc': { N: '1' } } } },
        { Delete: { TableName: t, Key: pk('d') } },
      ],
    }));

    assert.equal((await get('b')).val.S, 'new');
    assert.equal((await get('c')).qty.N, '1');
    assert.equal(await get('d'), undefined);
    assert.equal((await get('a')).version.N, '1');
  });

  await check('cancellation with CancellationReasons by position', async () => {
    let error;
    try {
      await send(ctx.ddb, new TransactWriteItemsCommand({
        TransactItems: [
          { Put: { TableName: t, Item: { ...pk('x'), val: { S: '1' } } } },
          { ConditionCheck: { TableName: t, Key: pk('y'), ConditionExpression: 'version = :two', ExpressionAttributeValues: { ':two': { N: '2' } } } },
          { Put: { TableName: t, Item: { ...pk('z'), val: { S: '1' } } } },
        ],
      }));
    } catch (e) {
      error = e;
    }
    assert.equal(error?.name, 'TransactionCanceledException');
    const reasons = error.CancellationReasons;
    assert.equal(reasons.length, 3);
    assert.equal(reasons[0].Code, 'None');
    assert.equal(reasons[1].Code, 'ConditionalCheckFailed');
    assert.equal(reasons[2].Code, 'None');
    assert.equal(await get('x'), undefined, 'nothing is written when the transaction cancels');
    assert.equal(await get('z'), undefined);
  });

  await check('ClientRequestToken replay', async () => {
    const input = {
      TransactItems: [{ Update: { TableName: t, Key: pk('cnt'), UpdateExpression: 'ADD qty :inc', ExpressionAttributeValues: { ':inc': { N: '1' } } } }],
      ClientRequestToken: 'smoke-token-1',
    };
    await send(ctx.ddb, new TransactWriteItemsCommand(structuredClone(input)));
    await send(ctx.ddb, new TransactWriteItemsCommand(structuredClone(input)));
    assert.equal((await get('cnt')).qty.N, '1', 'the replay does not write a second time');

    let errorName;
    try {
      await send(ctx.ddb, new TransactWriteItemsCommand({
        TransactItems: [{ Update: { TableName: t, Key: pk('cnt'), UpdateExpression: 'ADD qty :inc', ExpressionAttributeValues: { ':inc': { N: '2' } } } }],
        ClientRequestToken: 'smoke-token-1',
      }));
    } catch (e) {
      errorName = e.name;
    }
    assert.equal(errorName, 'IdempotentParameterMismatchException');
  });

  await check('TransactGetItems', async () => {
    await send(ctx.ddb, new TransactGetItemsCommand({ TransactItems: [{ Get: { TableName: t, Key: pk('a') } }] }));
  });
});
