// Money transfer (ppmMoney): a TransactWriteItems that debits one account with
// a condition, credits another, and writes a transfer record. The same
// transaction is then attempted with a condition that cannot be met, so the
// transaction cancels and CancellationReasons describe each item.

import { GetCommand, PutCommand, TransactWriteCommand } from '@aws-sdk/lib-dynamodb';
import { simpleTable } from '../cases/shared.ts';
import type { Case } from '../cases/shared.ts';
import { deep, eq } from '../assert.ts';

const NAME = 'app-money-transfer';

function transferItems(table: string, from: string, to: string, txn: string, amount: number): Record<string, unknown>[] {
  return [
    {
      Update: {
        TableName: table,
        Key: { id: from },
        UpdateExpression: 'SET balance = balance - :amt',
        ConditionExpression: 'balance >= :amt',
        ExpressionAttributeValues: { ':amt': amount },
      },
    },
    {
      Update: {
        TableName: table,
        Key: { id: to },
        UpdateExpression: 'SET balance = balance + :amt',
        ExpressionAttributeValues: { ':amt': amount },
      },
    },
    {
      Put: {
        TableName: table,
        Item: { id: txn, from, to, amount },
      },
    },
  ];
}

export const moneyTransfer: Case = {
  name: NAME,
  requires: ['TransactWriteItems'],
  fn: async (ctx) => {
    const table = ctx.table('app-money');
    await ctx.createTable(simpleTable(table));

    await ctx.call(() => ctx.doc.send(new PutCommand({ TableName: table, Item: { id: 'acct-a', balance: 100 } })));
    await ctx.call(() => ctx.doc.send(new PutCommand({ TableName: table, Item: { id: 'acct-b', balance: 0 } })));

    // Transfer 40 from acct-a to acct-b: conditioned debit, credit, and the
    // transfer record.
    await ctx.call(() =>
      ctx.doc.send(
        new TransactWriteCommand({ TransactItems: transferItems(table, 'acct-a', 'acct-b', 'txn-1', 40) as never }),
      ),
    );

    // Read back directly so the extra requests are not recorded.
    const a = (await ctx.doc.send(new GetCommand({ TableName: table, Key: { id: 'acct-a' } }))) as {
      Item?: { balance?: number };
    };
    eq(NAME, a.Item?.balance, 60, 'acct-a balance after transfer');
    const b = (await ctx.doc.send(new GetCommand({ TableName: table, Key: { id: 'acct-b' } }))) as {
      Item?: { balance?: number };
    };
    eq(NAME, b.Item?.balance, 40, 'acct-b balance after transfer');
    const txn1 = (await ctx.doc.send(new GetCommand({ TableName: table, Key: { id: 'txn-1' } }))) as {
      Item?: unknown;
    };
    deep(NAME, txn1.Item, { id: 'txn-1', from: 'acct-a', to: 'acct-b', amount: 40 }, 'transfer record exists');

    // The same transfer for 1000: acct-a now holds 60, so the debit condition
    // fails and the whole transaction cancels.
    const failed = (await ctx.call(() =>
      ctx.doc.send(
        new TransactWriteCommand({ TransactItems: transferItems(table, 'acct-a', 'acct-b', 'txn-2', 1000) as never }),
      ),
    )) as { name?: string; CancellationReasons?: { Code?: string }[] };
    eq(NAME, failed.name, 'TransactionCanceledException', 'failing transaction cancels');
    deep(
      NAME,
      (failed.CancellationReasons ?? []).map((r) => r.Code),
      ['ConditionalCheckFailed', 'None', 'None'],
      'CancellationReasons by position',
    );

    // Nothing changed after the failing transaction.
    const a2 = (await ctx.doc.send(new GetCommand({ TableName: table, Key: { id: 'acct-a' } }))) as {
      Item?: { balance?: number };
    };
    eq(NAME, a2.Item?.balance, 60, 'acct-a unchanged after failed transaction');
    const b2 = (await ctx.doc.send(new GetCommand({ TableName: table, Key: { id: 'acct-b' } }))) as {
      Item?: { balance?: number };
    };
    eq(NAME, b2.Item?.balance, 40, 'acct-b unchanged after failed transaction');
    const txn2 = (await ctx.doc.send(new GetCommand({ TableName: table, Key: { id: 'txn-2' } }))) as {
      Item?: unknown;
    };
    eq(NAME, txn2.Item, undefined, 'no transfer record after failed transaction');
  },
};
