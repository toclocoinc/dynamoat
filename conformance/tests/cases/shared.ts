// Shared table definitions and the case type used by the suite.

import type { CaseSpec } from '../../src/harness.ts';

export type Case = CaseSpec;

// simpleTable is a hash-only table keyed by "id" (S), PAY_PER_REQUEST.
export function simpleTable(tableName: string): Record<string, unknown> {
  return {
    TableName: tableName,
    KeySchema: [{ AttributeName: 'id', KeyType: 'HASH' }],
    AttributeDefinitions: [{ AttributeName: 'id', AttributeType: 'S' }],
    BillingMode: 'PAY_PER_REQUEST',
  };
}

// tenantTable is a tenant shape: hash key id, PAY_PER_REQUEST,
// stream OLD_IMAGE, and two GSIs over entity + comboKey (one ALL projection,
// one INCLUDE [name]).
export function tenantTable(tableName: string): Record<string, unknown> {
  return {
    TableName: tableName,
    KeySchema: [{ AttributeName: 'id', KeyType: 'HASH' }],
    AttributeDefinitions: [
      { AttributeName: 'id', AttributeType: 'S' },
      { AttributeName: 'entity', AttributeType: 'S' },
      { AttributeName: 'comboKey', AttributeType: 'S' },
    ],
    BillingMode: 'PAY_PER_REQUEST',
    StreamSpecification: { StreamEnabled: true, StreamViewType: 'OLD_IMAGE' },
    GlobalSecondaryIndexes: [
      {
        IndexName: 'entity-comboKey-index',
        KeySchema: [
          { AttributeName: 'entity', KeyType: 'HASH' },
          { AttributeName: 'comboKey', KeyType: 'RANGE' },
        ],
        Projection: { ProjectionType: 'ALL' },
      },
      {
        IndexName: 'entity-comboKey-name-index',
        KeySchema: [
          { AttributeName: 'entity', KeyType: 'HASH' },
          { AttributeName: 'comboKey', KeyType: 'RANGE' },
        ],
        Projection: { ProjectionType: 'INCLUDE', NonKeyAttributes: ['name'] },
      },
    ],
  };
}
