import type { AttributeValue } from "@aws-sdk/client-dynamodb";

// An item is a DynamoDB item in the wire format the AWS SDK returns: attribute
// names to AttributeValue objects ("S", "N", "M", "L", ...). Numbers stay as
// their DynamoDB strings, so nothing here risks floating-point rounding.
export type Item = Record<string, AttributeValue>;

// A key is the subset of an item holding a table's or index's key attributes.
export type Key = Record<string, AttributeValue>;

export interface KeySchemaElement {
  AttributeName: string;
  KeyType: "HASH" | "RANGE";
}

export type ScalarAttributeType = "S" | "N" | "B";

export interface AttributeDefinition {
  AttributeName: string;
  AttributeType: ScalarAttributeType;
}

export interface Projection {
  ProjectionType: "ALL" | "KEYS_ONLY" | "INCLUDE";
  NonKeyAttributes?: string[];
}

export interface GSIDescription {
  IndexName: string;
  KeySchema: KeySchemaElement[];
  Projection: Projection;
}

export type StreamViewType = "KEYS_ONLY" | "NEW_IMAGE" | "OLD_IMAGE" | "NEW_AND_OLD_IMAGES";

export interface StreamSpecification {
  StreamEnabled: boolean;
  StreamViewType?: StreamViewType;
}

// TableDescription is the subset of DynamoDB's DescribeTable output that the
// import needs to recreate a table on the target.
export interface TableDescription {
  TableName: string;
  KeySchema: KeySchemaElement[];
  AttributeDefinitions: AttributeDefinition[];
  GlobalSecondaryIndexes?: GSIDescription[];
  StreamSpecification?: StreamSpecification;
  TableStatus?: string;
}

export interface TTLStatus {
  enabled: boolean;
  attributeName?: string;
}

export interface ScanRequest {
  table: string;
  segment?: number;
  totalSegments?: number;
  startKey?: Key;
  limit?: number;
}

export interface ScanPage {
  items: Item[];
  lastEvaluatedKey?: Key;
}
