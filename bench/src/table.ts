// Table setup and teardown: create the tenant table, wait until it
// and every GSI are ACTIVE, seed it, and drop it.
import { CreateTableCommand, DeleteTableCommand, DescribeTableCommand } from "@aws-sdk/client-dynamodb";
import { BatchWriteCommand, PutCommand } from "@aws-sdk/lib-dynamodb";
import { makeSeedItem, tableDefinition } from "./items.ts";
import { errorName, sleep } from "./util.ts";

type DocClient = import("@aws-sdk/lib-dynamodb").DynamoDBDocumentClient;
type Log = (msg: string) => void;

const BATCH_SIZE = 25; // DynamoDB's BatchWriteItem limit

export async function createTable(doc: DocClient, tableName: string): Promise<void> {
  await doc.send(new CreateTableCommand(tableDefinition(tableName) as never));
}

// waitActive polls DescribeTable until the table and every GSI are ACTIVE.
export async function waitActive(doc: DocClient, tableName: string, timeoutMs: number, log: Log): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const res = await doc.send(new DescribeTableCommand({ TableName: tableName }));
    const table = res.Table;
    const tableActive = table?.TableStatus === "ACTIVE";
    const gsis = table?.GlobalSecondaryIndexes ?? [];
    const gsisActive = gsis.every((g) => g.IndexStatus === "ACTIVE");
    if (tableActive && gsisActive) return;
    if (Date.now() > deadline) {
      throw new Error(`timed out waiting for table ${tableName} to become ACTIVE`);
    }
    await sleep(500);
  }
}

// seed writes seedItems with BatchWriteItem. dynamoat
// does not implement BatchWriteItem yet, so when it answers
// UnknownOperationException the seed falls back to one PutItem per item.
export async function seed(doc: DocClient, tableName: string, count: number, log: Log): Promise<void> {
  const items = Array.from({ length: count }, (_, i) => makeSeedItem(i).item);
  if (items.length === 0) return;
  try {
    await batchWrite(doc, tableName, items);
  } catch (e) {
    if (errorName(e) === "UnknownOperationException") {
      log("BatchWriteItem is not implemented by this server; seeding with PutItem instead");
      for (const item of items) {
        await doc.send(new PutCommand({ TableName: tableName, Item: item }));
      }
      return;
    }
    throw e;
  }
}

async function batchWrite(doc: DocClient, tableName: string, items: unknown[]): Promise<void> {
  let remaining = items;
  // Retry unprocessed items (throttling) a bounded number of times.
  for (let attempt = 0; attempt < 10 && remaining.length > 0; attempt++) {
    const next: unknown[] = [];
    for (let i = 0; i < remaining.length; i += BATCH_SIZE) {
      const res = await doc.send(
        new BatchWriteCommand({
          RequestItems: {
            [tableName]: remaining.slice(i, i + BATCH_SIZE).map((item) => ({ PutRequest: { Item: item } })),
          },
        }),
      );
      for (const req of res.UnprocessedItems?.[tableName] ?? []) {
        if (req.PutRequest?.Item) next.push(req.PutRequest.Item);
      }
    }
    remaining = next;
  }
  if (remaining.length > 0) {
    throw new Error(`BatchWriteItem left ${remaining.length} items unprocessed`);
  }
}

export async function deleteTable(doc: DocClient, tableName: string, log: Log): Promise<void> {
  try {
    await doc.send(new DeleteTableCommand({ TableName: tableName }));
  } catch (e) {
    log(`DeleteTable failed: ${errorName(e)}`);
  }
}
