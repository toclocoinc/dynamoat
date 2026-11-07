import assert from 'node:assert/strict';
import { test } from 'node:test';
import { cleanError, cleanResult, makeNormaliser } from '../src/normalise.ts';

test('normaliser strips the random table prefix', () => {
  const n = makeNormaliser({ prefix: 'conformance-abc123-' });
  assert.equal(n('conformance-abc123-tenant'), 'tenant');
  assert.equal(
    n('arn:aws:dynamodb:us-east-1:000000000000:table/conformance-abc123-tenant'),
    'arn:aws:dynamodb:«normalised»:«normalised»:table/tenant',
  );
});

test('normaliser normalises ARN account, region and stream label', () => {
  const n = makeNormaliser({});
  assert.deepEqual(
    n({
      TableArn: 'arn:aws:dynamodb:us-west-2:123456789012:table/t',
      LatestStreamArn: 'arn:aws:dynamodb:us-west-2:123456789012:table/t/stream/2026-11-06T16:17:00.000',
      IndexArn: 'arn:aws:dynamodb:us-west-2:123456789012:table/t/index/g',
    }),
    {
      TableArn: 'arn:aws:dynamodb:«normalised»:«normalised»:table/t',
      LatestStreamArn: 'arn:aws:dynamodb:«normalised»:«normalised»:table/t/stream/«normalised»',
      IndexArn: 'arn:aws:dynamodb:«normalised»:«normalised»:table/t/index/g',
    },
  );
});

test('normaliser normalises volatile metadata fields', () => {
  const n = makeNormaliser({});
  assert.deepEqual(
    n({
      TableId: 'a1b2c3',
      CreationDateTime: 1765380000.123,
      LastUpdateToPayPerRequestDateTime: 1765380000.123,
      LatestStreamLabel: '2026-11-06T16:17:00.000',
      requestId: 'ABC123',
      ExtendedRequestId: 'xyz',
    }),
    {
      TableId: '«normalised»',
      CreationDateTime: 0,
      LastUpdateToPayPerRequestDateTime: 0,
      LatestStreamLabel: '«normalised»',
      requestId: '«normalised»',
      ExtendedRequestId: '«normalised»',
    },
  );
});

test('normaliser converts binary values to base64', () => {
  const n = makeNormaliser({});
  assert.equal(n(Buffer.from('hi')), 'aGk=');
});

test('normaliser drops undefined object values so JSON round-trips unchanged', () => {
  const n = makeNormaliser({});
  assert.deepEqual(n({ a: 1, b: undefined }), { a: 1 });
});

test('cleanResult keeps only httpStatusCode in metadata', () => {
  assert.deepEqual(cleanResult({ Items: [{ a: 1 }], $metadata: { httpStatusCode: 200, requestId: 'x', attempts: 1 } }), {
    Items: [{ a: 1 }],
    $metadata: { httpStatusCode: 200 },
  });
});

test('cleanError captures name, message, status and extras', () => {
  assert.deepEqual(
    cleanError({
      name: 'ConditionalCheckFailedException',
      message: 'The conditional request failed',
      $metadata: { httpStatusCode: 400 },
      Item: { a: { S: 'x' } },
    }),
    {
      name: 'ConditionalCheckFailedException',
      message: 'The conditional request failed',
      $metadata: { httpStatusCode: 400 },
      Item: { a: { S: 'x' } },
    },
  );
});

test('normaliser strips the prefix from object keys (BatchGetItem Responses)', () => {
  const n = makeNormaliser({ prefix: 'conformance-abc123-' });
  assert.deepEqual(
    n({ Responses: { 'conformance-abc123-batchget': [{ id: 'b1' }] } }),
    { Responses: { batchget: [{ id: 'b1' }] } },
  );
});

test('normaliser replaces the opaque PartiQL NextToken', () => {
  const n = makeNormaliser({});
  assert.deepEqual(n({ NextToken: 'opaque-token' }), { NextToken: '«normalised»' });
});

test('normaliser sorts global secondary indexes by IndexName', () => {
  const n = makeNormaliser({});
  assert.deepEqual(
    n({
      Table: {
        GlobalSecondaryIndexes: [
          { IndexName: 'z-index', KeySchema: [] },
          { IndexName: 'a-index', KeySchema: [] },
        ],
      },
    }),
    {
      Table: {
        GlobalSecondaryIndexes: [
          { IndexName: 'a-index', KeySchema: [] },
          { IndexName: 'z-index', KeySchema: [] },
        ],
      },
    },
  );
});

test('normaliser sorts replicas by RegionName', () => {
  const n = makeNormaliser({});
  assert.deepEqual(
    n({ Replicas: [{ RegionName: 'us-east-1' }, { RegionName: 'eu-west-1' }] }),
    { Replicas: [{ RegionName: 'eu-west-1' }, { RegionName: 'us-east-1' }] },
  );
});

test('normaliser sorts BatchGetItem Responses lists by a stable JSON form', () => {
  const n = makeNormaliser({});
  assert.deepEqual(
    n({ Responses: { t: [{ id: 'b2', value: 'two' }, { id: 'b1', value: 'one' }] } }),
    { Responses: { t: [{ id: 'b1', value: 'one' }, { id: 'b2', value: 'two' }] } },
  );
});
