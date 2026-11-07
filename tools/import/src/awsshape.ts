import type { TableDescription as AwsTableDescription } from "@aws-sdk/client-dynamodb";
import type { TableDescription } from "./types";

// fromAwsTable keeps only the fields the import uses and normalizes the shape
// the AWS SDK returns (which is also dynamoat's DescribeTable shape) into the
// tool's own TableDescription.
export function fromAwsTable(t: AwsTableDescription): TableDescription {
  return {
    TableName: t.TableName ?? "",
    KeySchema: (t.KeySchema ?? []).map((k) => ({
      AttributeName: k.AttributeName ?? "",
      KeyType: k.KeyType as "HASH" | "RANGE",
    })),
    AttributeDefinitions: (t.AttributeDefinitions ?? []).map((d) => ({
      AttributeName: d.AttributeName ?? "",
      AttributeType: (d.AttributeType ?? "S") as "S" | "N" | "B",
    })),
    GlobalSecondaryIndexes: (t.GlobalSecondaryIndexes ?? []).map((g) => ({
      IndexName: g.IndexName ?? "",
      KeySchema: (g.KeySchema ?? []).map((k) => ({
        AttributeName: k.AttributeName ?? "",
        KeyType: k.KeyType as "HASH" | "RANGE",
      })),
      Projection: {
        ProjectionType: (g.Projection?.ProjectionType ?? "ALL") as "ALL" | "KEYS_ONLY" | "INCLUDE",
        ...(g.Projection?.NonKeyAttributes?.length
          ? { NonKeyAttributes: g.Projection.NonKeyAttributes }
          : {}),
      },
    })),
    StreamSpecification: t.StreamSpecification
      ? {
          StreamEnabled: t.StreamSpecification.StreamEnabled ?? false,
          StreamViewType: t.StreamSpecification.StreamViewType,
        }
      : undefined,
    TableStatus: t.TableStatus,
  };
}
