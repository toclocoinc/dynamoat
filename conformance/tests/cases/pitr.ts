// Point-in-time recovery: enable, write, change, restore to before the change,
// and compare. The waits are short because dynamoat's recovery window trails
// now by five seconds; real DynamoDB trails by minutes.

import {
  DescribeContinuousBackupsCommand,
  DescribeTableCommand,
  GetItemCommand,
  PutItemCommand,
  RestoreTableToPointInTimeCommand,
  UpdateContinuousBackupsCommand,
} from '@aws-sdk/client-dynamodb';
import { deep } from '../assert.ts';
import { simpleTable } from './shared.ts';
import type { Case } from './shared.ts';
import { callRetrying } from '../../src/harness.ts';
import type { Ctx } from '../../src/harness.ts';

function wait(seconds: number): Promise<void> {
  return new Promise((r) => setTimeout(r, seconds * 1000));
}

// waitEarliestRestorable polls until real DynamoDB has opened the recovery
// window (EarliestRestorableDateTime is set), which is not immediate after
// enabling PITR. dynamoat opens it at once.
async function waitEarliestRestorable(ctx: Ctx, table: string): Promise<void> {
  const deadline = Date.now() + 20 * 60 * 1000;
  while (Date.now() < deadline) {
    const res = (await ctx.ddb.send(new DescribeContinuousBackupsCommand({ TableName: table }))) as {
      ContinuousBackupsDescription?: { PointInTimeRecoveryDescription?: { EarliestRestorableDateTime?: Date } };
    };
    const earliest = res.ContinuousBackupsDescription?.PointInTimeRecoveryDescription?.EarliestRestorableDateTime;
    if (earliest && new Date(earliest).getTime() > 0) return;
    await wait(15);
  }
  throw new Error('PITR did not become restorable within 20 minutes');
}

// waitLatestRestorable polls until real DynamoDB's LatestRestorableDateTime
// has passed the restore point (capped at 15 minutes).
async function waitLatestRestorable(ctx: Ctx, table: string, point: Date): Promise<void> {
  const deadline = Date.now() + 15 * 60 * 1000;
  while (Date.now() < deadline) {
    const res = (await ctx.ddb.send(new DescribeContinuousBackupsCommand({ TableName: table }))) as {
      ContinuousBackupsDescription?: { PointInTimeRecoveryDescription?: { LatestRestorableDateTime?: Date } };
    };
    const latest = res.ContinuousBackupsDescription?.PointInTimeRecoveryDescription?.LatestRestorableDateTime;
    if (latest && new Date(latest).getTime() > point.getTime()) return;
    await wait(15);
  }
  throw new Error('LatestRestorableDateTime did not pass the restore point within 15 minutes');
}

// waitRestoredActive polls the restored table to ACTIVE without capturing the
// probes. Real DynamoDB can take up to 30 minutes; dynamoat is immediate.
async function waitRestoredActive(ctx: Ctx, table: string): Promise<void> {
  const cap = ctx.target.kind === 'aws' ? 30 * 60 * 1000 : 30000;
  const deadline = Date.now() + cap;
  for (;;) {
    const d = (await ctx.ddb.send(new DescribeTableCommand({ TableName: table }))) as {
      Table?: { TableStatus?: string };
    };
    if (d.Table?.TableStatus === 'ACTIVE') return;
    if (Date.now() > deadline) throw new Error('restored table did not become ACTIVE');
    await wait(ctx.target.kind === 'aws' ? 10 : 0.2);
  }
}

export const pitr: Case[] = [
  {
    name: 'pitr-restore',
    requires: ['UpdateContinuousBackups', 'DescribeContinuousBackups', 'RestoreTableToPointInTime'],
    slow: true,
    fn: async (ctx) => {
      const src = ctx.table('pitr-src');
      const dst = ctx.table('pitr-dst');
      await ctx.createTable(simpleTable(src));

      // Real DynamoDB refuses to enable PITR for a short while after a table is
      // created (ContinuousBackupsUnavailableException); retry until it accepts.
      await callRetrying(
        ctx,
        'UpdateContinuousBackups',
        () =>
          ctx.ddb.send(
            new UpdateContinuousBackupsCommand({
              TableName: src,
              PointInTimeRecoverySpecification: { PointInTimeRecoveryEnabled: true, RecoveryPeriodInDays: 35 },
            }),
          ),
        ['ContinuousBackupsUnavailableException'],
        5 * 60 * 1000,
      );

      // Real DynamoDB does not make a table restorable immediately after
      // enabling PITR; wait for the recovery window before choosing the
      // restore time.
      if (ctx.target.kind === 'aws') {
        await waitEarliestRestorable(ctx, src);
      }

      await ctx.call(() => ctx.ddb.send(new DescribeContinuousBackupsCommand({ TableName: src })));

      await ctx.call(() =>
        ctx.ddb.send(new PutItemCommand({ TableName: src, Item: { id: { S: 'r1' }, v: { S: 'before' } } })),
      );

      // Wait past the recovery trail, then change the item.
      await wait(6);
      const mark = new Date(Date.now() + 500);
      await wait(1);
      await ctx.call(() =>
        ctx.ddb.send(new PutItemCommand({ TableName: src, Item: { id: { S: 'r1' }, v: { S: 'after' } } })),
      );
      await wait(6);

      // Real DynamoDB only restores up to LatestRestorableDateTime, which
      // trails the present by about five minutes.
      if (ctx.target.kind === 'aws') {
        await waitLatestRestorable(ctx, src, mark);
      }

      await ctx.call(() =>
        ctx.ddb.send(
          new RestoreTableToPointInTimeCommand({
            SourceTableName: src,
            TargetTableName: dst,
            RestoreDateTime: mark,
          }),
        ),
      );

      await waitRestoredActive(ctx, dst);

      const got = (await ctx.call(() =>
        ctx.ddb.send(new GetItemCommand({ TableName: dst, Key: { id: { S: 'r1' } } })),
      )) as { Item?: { v?: { S?: string } } };
      deep(
        'pitr-restore',
        got.Item,
        { id: { S: 'r1' }, v: { S: 'before' } },
        'restored table holds the item as it was before the change',
      );
    },
  },
];
