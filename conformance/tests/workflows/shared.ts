// Helpers shared by the access-pattern cases.

// appTenantTable is a tenant shape: hash key id,
// PAY_PER_REQUEST, stream OLD_IMAGE, and the four GSIs the Lambdas use. The
// shared tenantTable in ../cases/shared.ts has only the first two indexes, so
// the workflow cases define their own.
export function appTenantTable(tableName: string): Record<string, unknown> {
  return {
    TableName: tableName,
    KeySchema: [{ AttributeName: 'id', KeyType: 'HASH' }],
    AttributeDefinitions: [
      { AttributeName: 'id', AttributeType: 'S' },
      { AttributeName: 'entity', AttributeType: 'S' },
      { AttributeName: 'comboKey', AttributeType: 'S' },
      { AttributeName: 'subscription', AttributeType: 'S' },
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
      {
        IndexName: 'comboKey-index',
        KeySchema: [{ AttributeName: 'comboKey', KeyType: 'HASH' }],
        Projection: { ProjectionType: 'ALL' },
      },
      {
        IndexName: 'subscription-entity-index',
        KeySchema: [
          { AttributeName: 'subscription', KeyType: 'HASH' },
          { AttributeName: 'entity', KeyType: 'RANGE' },
        ],
        Projection: { ProjectionType: 'ALL' },
      },
    ],
  };
}

// chunk splits a list into pages of at most size elements, matching the
// batching helpers a client uses for BatchGetItem (100 keys) and
// BatchWriteItem (25 writes).
export function chunk<T>(items: T[], size: number): T[][] {
  const out: T[][] = [];
  for (let i = 0; i < items.length; i += size) {
    out.push(items.slice(i, i + size));
  }
  return out;
}
