import {
  BatchWriteItemCommand,
  CreateTableCommand,
  DescribeTableCommand,
  ScanCommand,
  UpdateTimeToLiveCommand,
  type DynamoDBClient,
} from "@aws-sdk/client-dynamodb";
import { fromAwsTable } from "./awsshape";
import { batchWriteWithRetry } from "./batch";
import { isError } from "./errors";
import type { TableWriter } from "./interfaces";
import type {
  Item,
  Key,
  ScanPage,
  ScanRequest,
  TableDescription,
} from "./types";

// DynamoTableWriter recreates and fills a table on a dynamoat server.
export class DynamoTableWriter implements TableWriter {
  constructor(private readonly client: DynamoDBClient) {}

  async createTable(table: TableDescription): Promise<void> {
    await this.client.send(
      new CreateTableCommand({
        TableName: table.TableName,
        KeySchema: table.KeySchema,
        AttributeDefinitions: table.AttributeDefinitions,
        BillingMode: "PAY_PER_REQUEST",
        ...(table.GlobalSecondaryIndexes?.length
          ? {
              GlobalSecondaryIndexes: table.GlobalSecondaryIndexes.map((g) => ({
                IndexName: g.IndexName,
                KeySchema: g.KeySchema,
                Projection: {
                  ProjectionType: g.Projection.ProjectionType,
                  ...(g.Projection.NonKeyAttributes?.length
                    ? { NonKeyAttributes: g.Projection.NonKeyAttributes }
                    : {}),
                },
              })),
            }
          : {}),
        ...(table.StreamSpecification?.StreamEnabled &&
        table.StreamSpecification.StreamViewType
          ? {
              StreamSpecification: {
                StreamEnabled: true,
                StreamViewType: table.StreamSpecification.StreamViewType,
              },
            }
          : {}),
      }),
    );
  }

  async describeTable(name: string): Promise<TableDescription | undefined> {
    try {
      const out = await this.client.send(new DescribeTableCommand({ TableName: name }));
      return out.Table ? fromAwsTable(out.Table) : undefined;
    } catch (err) {
      if (isError(err, "ResourceNotFoundException")) {
        return undefined;
      }
      throw err;
    }
  }

  async updateTimeToLive(name: string, attributeName: string, enabled: boolean): Promise<void> {
    await this.client.send(
      new UpdateTimeToLiveCommand({
        TableName: name,
        TimeToLiveSpecification: { AttributeName: attributeName, Enabled: enabled },
      }),
    );
  }

  async writeItems(name: string, items: Item[]): Promise<void> {
    await batchWriteWithRetry((chunk) => this.batchWrite(name, chunk), items);
  }

  private async batchWrite(name: string, items: Item[]): Promise<Item[]> {
    const out = await this.client.send(
      new BatchWriteItemCommand({
        RequestItems: {
          [name]: items.map((item) => ({ PutRequest: { Item: item } })),
        },
      }),
    );
    return (out.UnprocessedItems?.[name] ?? [])
      .filter((w) => w.PutRequest)
      .map((w) => w.PutRequest!.Item as Item);
  }

  async scan(req: ScanRequest): Promise<ScanPage> {
    const out = await this.client.send(
      new ScanCommand({
        TableName: req.table,
        ...(req.segment !== undefined ? { Segment: req.segment } : {}),
        ...(req.totalSegments !== undefined ? { TotalSegments: req.totalSegments } : {}),
        ...(req.startKey ? { ExclusiveStartKey: req.startKey } : {}),
        Limit: req.limit ?? 1000,
        ConsistentRead: true,
      }),
    );
    return {
      items: (out.Items ?? []) as Item[],
      lastEvaluatedKey: out.LastEvaluatedKey as Key | undefined,
    };
  }
}
