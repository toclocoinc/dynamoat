import {
  DescribeTableCommand,
  DescribeTimeToLiveCommand,
  ListTablesCommand,
  ScanCommand,
  type DynamoDBClient,
} from "@aws-sdk/client-dynamodb";
import { fromAwsTable } from "./awsshape";
import type { TableReader } from "./interfaces";
import type {
  Item,
  Key,
  ScanPage,
  ScanRequest,
  TableDescription,
  TTLStatus,
} from "./types";

// DynamoTableReader reads from a DynamoDB endpoint (AWS DynamoDB or another
// dynamoat server) using the AWS SDK.
export class DynamoTableReader implements TableReader {
  constructor(private readonly client: DynamoDBClient) {}

  async listTables(): Promise<string[]> {
    const names: string[] = [];
    let start: string | undefined;
    do {
      const out = await this.client.send(
        new ListTablesCommand({
          ...(start ? { ExclusiveStartTableName: start } : {}),
          Limit: 100,
        }),
      );
      names.push(...(out.TableNames ?? []));
      start = out.LastEvaluatedTableName;
    } while (start);
    return names;
  }

  async describeTable(name: string): Promise<TableDescription> {
    const out = await this.client.send(new DescribeTableCommand({ TableName: name }));
    if (!out.Table) {
      throw new Error(`DescribeTable returned no table for "${name}"`);
    }
    return fromAwsTable(out.Table);
  }

  async describeTimeToLive(name: string): Promise<TTLStatus> {
    const out = await this.client.send(new DescribeTimeToLiveCommand({ TableName: name }));
    const desc = out.TimeToLiveDescription;
    return {
      enabled: desc?.TimeToLiveStatus === "ENABLED",
      attributeName: desc?.AttributeName,
    };
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
