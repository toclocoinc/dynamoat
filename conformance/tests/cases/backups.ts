// On-demand backup cases: CreateBackup, ListBackups, RestoreTableFromBackup,
// DescribeBackup, DeleteBackup.

import {
  CreateBackupCommand,
  DeleteBackupCommand,
  DescribeBackupCommand,
  DescribeTableCommand,
  ListBackupsCommand,
  RestoreTableFromBackupCommand,
} from '@aws-sdk/client-dynamodb';
import { GetCommand, PutCommand } from '@aws-sdk/lib-dynamodb';
import { callRetrying } from '../../src/harness.ts';
import type { Ctx } from '../../src/harness.ts';
import { simpleTable } from './shared.ts';
import type { Case } from './shared.ts';
import { eq } from '../assert.ts';

const NAME = 'backups';

// Real DynamoDB takes minutes to create a backup and to restore from one;
// dynamoat is immediate.
function capMs(ctx: Ctx): number {
  return ctx.target.kind === 'aws' ? 30 * 60 * 1000 : 120000;
}

function pollMs(ctx: Ctx): number {
  return ctx.target.kind === 'aws' ? 10000 : 150;
}

async function waitBackupAvailable(ctx: Ctx, arn: string): Promise<void> {
  const deadline = Date.now() + capMs(ctx);
  while (Date.now() < deadline) {
    const res = (await ctx.ddb.send(new DescribeBackupCommand({ BackupArn: arn }))) as {
      BackupDescription?: { BackupDetails?: { BackupStatus?: string } };
    };
    if (res.BackupDescription?.BackupDetails?.BackupStatus === 'AVAILABLE') return;
    await new Promise((r) => setTimeout(r, pollMs(ctx)));
  }
  throw new Error(`backup ${arn} did not become AVAILABLE in time`);
}

async function waitTableActive(ctx: Ctx, tableName: string): Promise<void> {
  const deadline = Date.now() + capMs(ctx);
  while (Date.now() < deadline) {
    const res = (await ctx.ddb.send(new DescribeTableCommand({ TableName: tableName }))) as {
      Table?: { TableStatus?: string };
    };
    if (res.Table?.TableStatus === 'ACTIVE') return;
    await new Promise((r) => setTimeout(r, pollMs(ctx)));
  }
  throw new Error(`table ${tableName} did not become ACTIVE in time`);
}

export const backups: Case[] = [
  {
    name: 'backups',
    requires: ['CreateBackup', 'ListBackups', 'DescribeBackup', 'RestoreTableFromBackup', 'DeleteBackup'],
    slow: true,
    fn: async (ctx) => {
      const src = ctx.table('backup-src');
      const dst = ctx.table('backup-dst');
      await ctx.createTable(simpleTable(src));

      await ctx.call(() => ctx.doc.send(new PutCommand({ TableName: src, Item: { id: 'b1', value: 'one' } })));
      await ctx.call(() => ctx.doc.send(new PutCommand({ TableName: src, Item: { id: 'b2', value: 'two' } })));

      // CreateBackup right after CreateTable can fail while the table is new.
      const created = (await callRetrying(
        ctx,
        'CreateBackup',
        () => ctx.ddb.send(new CreateBackupCommand({ TableName: src, BackupName: 'backup-case' })),
        ['ContinuousBackupsUnavailableException', 'LimitExceededException'],
      )) as { BackupDetails?: { BackupArn?: string; BackupType?: string } };
      eq(NAME, created.BackupDetails?.BackupType, 'USER', 'backup type is USER');
      const arn = created.BackupDetails?.BackupArn;
      if (!arn) throw new Error('CreateBackup returned no BackupArn');
      ctx.target.trackBackup(arn);

      await waitBackupAvailable(ctx, arn);

      const listed = (await ctx.call(() => ctx.ddb.send(new ListBackupsCommand({ TableName: src })))) as {
        BackupSummaries?: { BackupArn?: string }[];
      };
      eq(NAME, listed.BackupSummaries?.some((b) => b.BackupArn === arn), true, 'backup appears in ListBackups');

      const restored = (await ctx.call(() =>
        ctx.ddb.send(new RestoreTableFromBackupCommand({ TargetTableName: dst, BackupArn: arn })),
      )) as {
        TableDescription?: { TableStatus?: string; RestoreSummary?: { SourceBackupArn?: string } };
      };
      eq(NAME, restored.TableDescription?.RestoreSummary?.SourceBackupArn, arn, 'restore summary source arn');
      await waitTableActive(ctx, dst);

      const b1 = (await ctx.call(() => ctx.doc.send(new GetCommand({ TableName: dst, Key: { id: 'b1' } })))) as {
        Item?: { value?: string };
      };
      eq(NAME, b1.Item?.value, 'one', 'restored item b1');

      const deleted = (await ctx.call(() => ctx.ddb.send(new DeleteBackupCommand({ BackupArn: arn })))) as {
        BackupDescription?: { BackupDetails?: { BackupStatus?: string } };
      };
      eq(NAME, deleted.BackupDescription?.BackupDetails?.BackupStatus, 'DELETED', 'backup deleted');
    },
  },
];
