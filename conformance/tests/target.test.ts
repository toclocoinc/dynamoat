// Unit tests for the AWS cleanup in src/target.ts, driven through a fake
// DynamoDBClient so no credentials or network are needed.

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { cleanupAwsResources, setCleanupWaitForTest } from '../src/target.ts';

interface Call {
  name: string;
  input: Record<string, unknown>;
}

class FakeDdb {
  calls: Call[] = [];
  nextError: { command: string; error: unknown } | null = null;
  responses = new Map<string, unknown>();

  async send(command: unknown): Promise<unknown> {
    const cmd = command as { constructor?: { name?: string }; input?: Record<string, unknown> };
    const name = cmd.constructor?.name ?? '';
    this.calls.push({ name, input: cmd.input ?? {} });
    if (this.nextError && this.nextError.command === name) {
      const err = this.nextError.error;
      this.nextError = null;
      throw err;
    }
    return this.responses.get(name);
  }
}

function resourceNotFound(): unknown {
  return Object.assign(new Error('Requested resource not found'), { name: 'ResourceNotFoundException' });
}

test('cleanup removes replicas before deleting a table, then deletes backups', async () => {
  const ddb = new FakeDdb();
  ddb.responses.set('DescribeTableCommand', {
    Table: { Replicas: [{ RegionName: 'eu-west-1', RegionStatus: 'ACTIVE' }] },
  });

  await cleanupAwsResources(ddb, ['t1'], ['arn:backup-1']);

  assert.deepEqual(
    ddb.calls.map((c) => c.name),
    ['DescribeTableCommand', 'UpdateTableCommand', 'DeleteTableCommand', 'DeleteBackupCommand'],
  );
  assert.deepEqual(ddb.calls[1].input, {
    TableName: 't1',
    ReplicaUpdates: [{ Delete: { RegionName: 'eu-west-1' } }],
  });
  assert.deepEqual(ddb.calls[2].input, { TableName: 't1' });
  assert.deepEqual(ddb.calls[3].input, { BackupArn: 'arn:backup-1' });
});

test('cleanup ignores ResourceNotFoundException on delete and continues', async () => {
  const ddb = new FakeDdb();
  ddb.nextError = { command: 'DeleteTableCommand', error: resourceNotFound() };

  await cleanupAwsResources(ddb, ['a', 'b'], []);

  const deletes = ddb.calls.filter((c) => c.name === 'DeleteTableCommand').map((c) => c.input.TableName);
  assert.deepEqual(deletes, ['a', 'b']);
});

test('cleanup skips a table that is already gone', async () => {
  const ddb = new FakeDdb();
  ddb.nextError = { command: 'DescribeTableCommand', error: resourceNotFound() };

  await cleanupAwsResources(ddb, ['gone'], []);

  assert.deepEqual(ddb.calls.map((c) => c.name), ['DescribeTableCommand']);
});

test('cleanup logs a non-ResourceNotFound error to stderr and continues', async () => {
  const ddb = new FakeDdb();
  ddb.nextError = { command: 'DeleteTableCommand', error: new Error('boom') };

  const logged: unknown[][] = [];
  const orig = console.error;
  console.error = (...args: unknown[]) => {
    logged.push(args);
  };
  try {
    await cleanupAwsResources(ddb, ['a', 'b'], []);
  } finally {
    console.error = orig;
  }

  const deletes = ddb.calls.filter((c) => c.name === 'DeleteTableCommand').map((c) => c.input.TableName);
  assert.deepEqual(deletes, ['a', 'b']);
  assert.ok(logged.some((args) => args.some((a) => String(a).includes('delete table a'))));
});

test('cleanup waits for a table that is still being restored before deleting it and its backup', async () => {
  setCleanupWaitForTest(5000, 1);
  const ddb = new FakeDdb();
  const states = ['CREATING', 'CREATING', 'ACTIVE'];
  const send = ddb.send.bind(ddb);
  ddb.send = async (command: unknown) => {
    const name = (command as { constructor?: { name?: string } }).constructor?.name;
    if (name === 'DescribeTableCommand') ddb.responses.set(name, { Table: { TableStatus: states.shift() ?? 'ACTIVE' } });
    return send(command);
  };

  await cleanupAwsResources(ddb, ['restored'], ['arn:backup-1']);

  assert.deepEqual(
    ddb.calls.map((c) => c.name),
    ['DescribeTableCommand', 'DescribeTableCommand', 'DescribeTableCommand', 'DeleteTableCommand', 'DeleteBackupCommand'],
  );
});
