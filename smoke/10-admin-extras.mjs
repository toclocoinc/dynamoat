// 10-admin-extras: tags, continuous backups and on-demand backups. dynamoat
// does not implement these yet, so against dynamoat the checks report
// unsupported; the calls are written the way real DynamoDB expects them.

import assert from 'node:assert/strict';
import {
  CreateTableCommand,
  DescribeTableCommand,
  ListTagsOfResourceCommand,
  TagResourceCommand,
  UntagResourceCommand,
  DescribeContinuousBackupsCommand,
  UpdateContinuousBackupsCommand,
  CreateBackupCommand,
  ListBackupsCommand,
} from '@aws-sdk/client-dynamodb';
import { run, check, table, send, waitActive } from './lib.mjs';

const key = (pk) => [{ AttributeName: pk, KeyType: 'HASH' }];
const def = (pk, type = 'S') => [{ AttributeName: pk, AttributeType: type }];

await run('10-admin-extras', async (ctx) => {
  const t = table('admin');
  await send(ctx.ddb, new CreateTableCommand({
    TableName: t,
    KeySchema: key('pk'),
    AttributeDefinitions: def('pk'),
    BillingMode: 'PAY_PER_REQUEST',
  }));
  await waitActive(ctx.ddb, t);

  const desc = await send(ctx.ddb, new DescribeTableCommand({ TableName: t }));
  const arn = desc.Table.TableArn;

  await check('ListTagsOfResource on the table ARN', async () => {
    const res = await send(ctx.ddb, new ListTagsOfResourceCommand({ ResourceArn: arn }));
    assert.ok(Array.isArray(res.Tags));
  });

  await check('TagResource and UntagResource', async () => {
    await send(ctx.ddb, new TagResourceCommand({ ResourceArn: arn, Tags: [{ Key: 'env', Value: 'smoke' }] }));
    const tagged = await send(ctx.ddb, new ListTagsOfResourceCommand({ ResourceArn: arn }));
    assert.ok((tagged.Tags ?? []).some((t) => t.Key === 'env' && t.Value === 'smoke'));

    await send(ctx.ddb, new UntagResourceCommand({ ResourceArn: arn, TagKeys: ['env'] }));
    const untagged = await send(ctx.ddb, new ListTagsOfResourceCommand({ ResourceArn: arn }));
    assert.equal((untagged.Tags ?? []).some((t) => t.Key === 'env'), false);
  });

  await check('DescribeContinuousBackups', async () => {
    const res = await send(ctx.ddb, new DescribeContinuousBackupsCommand({ TableName: t }));
    assert.ok(res.ContinuousBackupsDescription);
  });

  await check('UpdateContinuousBackups', async () => {
    const res = await send(ctx.ddb, new UpdateContinuousBackupsCommand({
      TableName: t,
      PointInTimeRecoverySpecification: { PointInTimeRecoveryEnabled: true },
    }));
    assert.ok(res.ContinuousBackupsDescription);
  });

  await check('CreateBackup and ListBackups', async () => {
    const backup = await send(ctx.ddb, new CreateBackupCommand({ TableName: t, BackupName: 'smoke-backup' }));
    assert.ok(backup.BackupDetails.BackupArn);
    const listed = await send(ctx.ddb, new ListBackupsCommand({ TableName: t }));
    assert.ok((listed.BackupSummaries ?? []).some((b) => b.BackupName === 'smoke-backup'));
  });
});
