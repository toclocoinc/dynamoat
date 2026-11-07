// Cases for operations not merged into dynamoat yet. Each carries a `requires`
// list so the suite skips it (with a message) when the server is missing the
// operation; against real DynamoDB they all run.

import { BatchGetCommand, BatchWriteCommand, GetCommand, PutCommand, QueryCommand, ScanCommand, TransactGetCommand, TransactWriteCommand, UpdateCommand } from '@aws-sdk/lib-dynamodb';
import { captureScan } from '../../src/harness.ts';
import { simpleTable, tenantTable } from './shared.ts';
import type { Case } from './shared.ts';
import { deep, eq, setEq } from '../assert.ts';

export const unmerged: Case[] = [
  {
    name: 'update-item',
    requires: ['UpdateItem'],
    fn: async (ctx) => {
      const table = ctx.table('updateitem');
      await ctx.createTable(simpleTable(table));

      await ctx.call(() =>
        ctx.doc.send(new PutCommand({ TableName: table, Item: { id: 'u1', count: 1, name: 'old', temp: 'x' } })),
      );
      // SET with the default ReturnValues NONE.
      const setNone = await ctx.call(() =>
        ctx.doc.send(
          new UpdateCommand({
            TableName: table,
            Key: { id: 'u1' },
            UpdateExpression: 'SET #n = :n',
            ExpressionAttributeNames: { '#n': 'name' },
            ExpressionAttributeValues: { ':n': 'new' },
          }),
        ),
      );
      eq('update-item', (setNone as { Attributes?: unknown }).Attributes, undefined, 'ReturnValues NONE omits Attributes');

      // ADD with UPDATED_OLD.
      const addOld = await ctx.call(() =>
        ctx.doc.send(
          new UpdateCommand({
            TableName: table,
            Key: { id: 'u1' },
            UpdateExpression: 'ADD #c :c',
            ExpressionAttributeNames: { '#c': 'count' },
            ExpressionAttributeValues: { ':c': 2 },
            ReturnValues: 'UPDATED_OLD',
          }),
        ),
      );
      deep('update-item', (addOld as { Attributes?: unknown }).Attributes, { count: 1 }, 'UPDATED_OLD returns the old value');

      // REMOVE with UPDATED_NEW.
      const removeNew = await ctx.call(() =>
        ctx.doc.send(
          new UpdateCommand({
            TableName: table,
            Key: { id: 'u1' },
            UpdateExpression: 'REMOVE #t',
            ExpressionAttributeNames: { '#t': 'temp' },
            ReturnValues: 'UPDATED_NEW',
          }),
        ),
      );
      deep('update-item', (removeNew as { Attributes?: unknown }).Attributes, undefined, 'UPDATED_NEW after only a REMOVE has no Attributes');

      // SET with ALL_OLD.
      const allOld = await ctx.call(() =>
        ctx.doc.send(
          new UpdateCommand({
            TableName: table,
            Key: { id: 'u1' },
            UpdateExpression: 'SET #n = :n',
            ExpressionAttributeNames: { '#n': 'name' },
            ExpressionAttributeValues: { ':n': 'newer' },
            ReturnValues: 'ALL_OLD',
          }),
        ),
      );
      deep('update-item', (allOld as { Attributes?: unknown }).Attributes, { id: 'u1', count: 3, name: 'new' }, 'ALL_OLD returns the item before the update');

      // SET with ALL_NEW.
      const allNew = await ctx.call(() =>
        ctx.doc.send(
          new UpdateCommand({
            TableName: table,
            Key: { id: 'u1' },
            UpdateExpression: 'SET #n = :n',
            ExpressionAttributeNames: { '#n': 'name' },
            ExpressionAttributeValues: { ':n': 'newest' },
            ReturnValues: 'ALL_NEW',
          }),
        ),
      );
      deep('update-item', (allNew as { Attributes?: unknown }).Attributes, { id: 'u1', count: 3, name: 'newest' }, 'ALL_NEW returns the item after the update');
    },
  },
  {
    name: 'query-gsi-begins-with-paging',
    requires: ['Query'],
    fn: async (ctx) => {
      const table = ctx.table('query');
      await ctx.createTable(tenantTable(table));

      for (const item of [
        { id: 'q1', entity: 'acme', comboKey: 'order-1', name: 'one' },
        { id: 'q2', entity: 'acme', comboKey: 'order-2', name: 'two' },
        { id: 'q3', entity: 'acme', comboKey: 'order-3', name: 'three' },
        { id: 'q4', entity: 'other', comboKey: 'order-1', name: 'four' },
      ]) {
        await ctx.call(() => ctx.doc.send(new PutCommand({ TableName: table, Item: item })));
      }

      // Real DynamoDB updates a GSI asynchronously: wait until the writes are
      // visible in the index (uncaptured) before paging it.
      if (ctx.target.kind === 'aws') {
        const deadline = Date.now() + 120000;
        for (;;) {
          const probe = (await ctx.doc.send(
            new QueryCommand({
              TableName: table,
              IndexName: 'entity-comboKey-index',
              KeyConditionExpression: 'entity = :e AND begins_with(comboKey, :p)',
              ExpressionAttributeValues: { ':e': 'acme', ':p': 'order-' },
              Select: 'COUNT',
            }),
          )) as { Count?: number };
          if (probe.Count === 3) break;
          if (Date.now() > deadline) throw new Error('GSI did not show the writes within 120s');
          await new Promise((r) => setTimeout(r, 1000));
        }
      }

      // Page the GSI with begins_with on the range key.
      const seen: { id: string; comboKey: string }[] = [];
      const pages: unknown[][] = [];
      let last: unknown;
      for (let i = 0; i < 10; i++) {
        const params: Record<string, unknown> = {
          TableName: table,
          IndexName: 'entity-comboKey-index',
          KeyConditionExpression: 'entity = :e AND begins_with(comboKey, :p)',
          ExpressionAttributeValues: { ':e': 'acme', ':p': 'order-' },
          Limit: 1,
        };
        if (last) params.ExclusiveStartKey = last;
        // Plain sends: whether the final full page carries a LastEvaluatedKey
        // (followed by an empty page) is not defined; DynamoDB has done both.
        const res = (await ctx.doc.send(new QueryCommand(params as never))) as {
          Items?: { id: string; comboKey: string }[];
          LastEvaluatedKey?: unknown;
        };
        if ((res.Items ?? []).length > 0) pages.push(res.Items ?? []);
        seen.push(...(res.Items ?? []));
        last = res.LastEvaluatedKey;
        if (!last) break;
      }
      ctx.capture('Query pages', { Pages: pages });

      eq('query-gsi-begins-with-paging', seen.length, 3, 'begins_with page count');
      setEq('query-gsi-begins-with-paging', seen.map((i) => i.id), ['q1', 'q2', 'q3'], 'begins_with returns only the prefix');
      for (const item of seen) {
        eq('query-gsi-begins-with-paging', item.comboKey.startsWith('order-'), true, `comboKey prefix for ${item.id}`);
      }
    },
  },
  {
    name: 'query-lsi-consistent-read',
    requires: ['Query'],
    fn: async (ctx) => {
      const table = ctx.table('lsi-query');
      await ctx.createTable({
        TableName: table,
        KeySchema: [
          { AttributeName: 'id', KeyType: 'HASH' },
          { AttributeName: 'sk', KeyType: 'RANGE' },
        ],
        AttributeDefinitions: [
          { AttributeName: 'id', AttributeType: 'S' },
          { AttributeName: 'sk', AttributeType: 'S' },
          { AttributeName: 'year', AttributeType: 'N' },
        ],
        BillingMode: 'PAY_PER_REQUEST',
        LocalSecondaryIndexes: [
          {
            IndexName: 'year-index',
            KeySchema: [
              { AttributeName: 'id', KeyType: 'HASH' },
              { AttributeName: 'year', KeyType: 'RANGE' },
            ],
            Projection: { ProjectionType: 'ALL' },
          },
        ],
      });

      await ctx.call(() => ctx.doc.send(new PutCommand({ TableName: table, Item: { id: 'p1', sk: 'a', year: 2001 } })));
      await ctx.call(() => ctx.doc.send(new PutCommand({ TableName: table, Item: { id: 'p1', sk: 'b', year: 2000 } })));

      const res = (await ctx.call(() =>
        ctx.doc.send(
          new QueryCommand({
            TableName: table,
            IndexName: 'year-index',
            KeyConditionExpression: 'id = :i',
            ExpressionAttributeValues: { ':i': 'p1' },
            ConsistentRead: true,
          }),
        ),
      )) as { Items?: { year?: number }[]; Count?: number };

      eq('query-lsi-consistent-read', res.Count, 2, 'LSI query returns both items');
      deep('query-lsi-consistent-read', res.Items?.map((i) => i.year), [2000, 2001], 'LSI query sort order');
    },
  },
  {
    name: 'scan-paging',
    requires: ['Scan'],
    fn: async (ctx) => {
      const table = ctx.table('scan');
      await ctx.createTable(simpleTable(table));

      for (let i = 1; i <= 3; i++) {
        await ctx.call(() => ctx.doc.send(new PutCommand({ TableName: table, Item: { id: `s${i}`, value: i } })));
      }

      const seen = await captureScan(ctx, 'scan-paging-items', async (last) => {
        const params: Record<string, unknown> = { TableName: table, Limit: 1 };
        if (last) params.ExclusiveStartKey = last;
        const res = (await ctx.doc.send(new ScanCommand(params as never))) as {
          Items?: { id: string; value: number }[];
          LastEvaluatedKey?: unknown;
        };
        return { items: res.Items ?? [], last: res.LastEvaluatedKey };
      });

      eq('scan-paging', seen.items.length, 3, 'scan visits every item once');
      setEq('scan-paging', seen.items.map((i) => (i as { id: string }).id), ['s1', 's2', 's3'], 'scan paging union');
    },
  },
  {
    name: 'batch-write-item',
    requires: ['BatchWriteItem'],
    fn: async (ctx) => {
      const table = ctx.table('batchwrite');
      await ctx.createTable(simpleTable(table));

      await ctx.call(() =>
        ctx.doc.send(
          new BatchWriteCommand({
            RequestItems: {
              [table]: [
                { PutRequest: { Item: { id: 'b1', value: 'one' } } },
                { PutRequest: { Item: { id: 'b2', value: 'two' } } },
              ],
            },
          }),
        ),
      );

      const check = (await ctx.doc.send(
        new BatchGetCommand({ RequestItems: { [table]: { Keys: [{ id: 'b1' }, { id: 'b2' }] } } }),
      )) as { Responses?: Record<string, { id: string; value: string }[]> };
      setEq('batch-write-item', (check.Responses?.[table] ?? []).map((i) => i.id), ['b1', 'b2'], 'BatchWrite wrote both items');
    },
  },
  {
    name: 'batch-get-item',
    requires: ['BatchGetItem'],
    fn: async (ctx) => {
      const table = ctx.table('batchget');
      await ctx.createTable(simpleTable(table));

      await ctx.call(() => ctx.doc.send(new PutCommand({ TableName: table, Item: { id: 'b1', value: 'one' } })));
      await ctx.call(() => ctx.doc.send(new PutCommand({ TableName: table, Item: { id: 'b2', value: 'two' } })));

      const got = (await ctx.call(() =>
        ctx.doc.send(
          new BatchGetCommand({
            RequestItems: {
              [table]: { Keys: [{ id: 'b1' }, { id: 'b2' }] },
            },
          }),
        ),
      )) as { Responses?: Record<string, { id: string; value: string }[]> };
      eq('batch-get-item', (got.Responses?.[table] ?? []).length, 2, 'BatchGet returns both items');
      setEq('batch-get-item', (got.Responses?.[table] ?? []).map((i) => i.id), ['b1', 'b2'], 'BatchGet ids');
      setEq('batch-get-item', (got.Responses?.[table] ?? []).map((i) => i.value), ['one', 'two'], 'BatchGet values');
    },
  },
  {
    name: 'transact-get-items',
    requires: ['TransactGetItems'],
    fn: async (ctx) => {
      const table = ctx.table('transactget');
      await ctx.createTable(simpleTable(table));

      await ctx.call(() => ctx.doc.send(new PutCommand({ TableName: table, Item: { id: 'g1', value: 'one' } })));
      await ctx.call(() => ctx.doc.send(new PutCommand({ TableName: table, Item: { id: 'g2', value: 'two' } })));

      const got = (await ctx.call(() =>
        ctx.doc.send(
          new TransactGetCommand({
            TransactItems: [
              { Get: { TableName: table, Key: { id: 'g1' }, ProjectionExpression: '#v', ExpressionAttributeNames: { '#v': 'value' } } },
              { Get: { TableName: table, Key: { id: 'missing' } } },
              { Get: { TableName: table, Key: { id: 'g2' } } },
            ],
          }),
        ),
      )) as { Responses?: { Item?: { id?: string; value?: string } }[] };

      eq('transact-get-items', got.Responses?.length, 3, 'three responses in request order');
      deep('transact-get-items', got.Responses?.[0]?.Item, { value: 'one' }, 'first item projected');
      eq('transact-get-items', got.Responses?.[1]?.Item, undefined, 'missing item has no Item');
      deep('transact-get-items', got.Responses?.[2]?.Item, { id: 'g2', value: 'two' }, 'third item full');
    },
  },
  {
    name: 'transact-write-success',
    requires: ['TransactWriteItems'],
    fn: async (ctx) => {
      const table = ctx.table('transact');
      await ctx.createTable(simpleTable(table));

      await ctx.call(() => ctx.doc.send(new PutCommand({ TableName: table, Item: { id: 't1', value: 'a' } })));

      await ctx.call(() =>
        ctx.doc.send(
          new TransactWriteCommand({
            TransactItems: [
              { Update: { TableName: table, Key: { id: 't1' }, UpdateExpression: 'SET value = :v', ExpressionAttributeValues: { ':v': 'a2' } } },
              { Put: { TableName: table, Item: { id: 't2', value: 'b' } } },
              { ConditionCheck: { TableName: table, Key: { id: 't1' }, ConditionExpression: 'attribute_exists(id)' } },
            ],
          }),
        ),
      );
    },
  },
  {
    name: 'transact-write-cancellation',
    requires: ['TransactWriteItems'],
    fn: async (ctx) => {
      const table = ctx.table('transact-cancel');
      await ctx.createTable(simpleTable(table));

      await ctx.call(() => ctx.doc.send(new PutCommand({ TableName: table, Item: { id: 't1', value: 'a' } })));

      const failed = await ctx.call(() =>
        ctx.doc.send(
          new TransactWriteCommand({
            TransactItems: [
              { Put: { TableName: table, Item: { id: 't2', value: 'b' } } },
              { ConditionCheck: { TableName: table, Key: { id: 't1' }, ConditionExpression: 'attribute_not_exists(id)' } },
            ],
          }),
        ),
      );
      eq('transact-write-cancellation', (failed as { name?: string }).name, 'TransactionCanceledException', 'transaction cancels');

      const t2 = (await ctx.doc.send(new GetCommand({ TableName: table, Key: { id: 't2' } }))) as { Item?: unknown };
      eq('transact-write-cancellation', t2.Item, undefined, 'cancelled Put wrote nothing');
      const t1 = (await ctx.doc.send(new GetCommand({ TableName: table, Key: { id: 't1' } }))) as { Item?: unknown };
      deep('transact-write-cancellation', t1.Item, { id: 't1', value: 'a' }, 'cancelled transaction left t1 unchanged');
    },
  },
];
