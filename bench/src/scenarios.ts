// The four benchmark scenarios, each one closed-loop request against the
// tenant table.
import { GetCommand, PutCommand, QueryCommand, UpdateCommand } from "@aws-sdk/lib-dynamodb";
import { comboKeyPrefix, makePutItem, STATUSES, subscriptionFor } from "./items.ts";

export interface BenchState {
  seedCount: number;
  boards: number;
  nextPutId: number;
  nextName: number;
}

export interface ScenarioCtx {
  doc: import("@aws-sdk/lib-dynamodb").DynamoDBDocumentClient;
  tableName: string;
  state: BenchState;
}

export interface Scenario {
  name: string;
  run(ctx: ScenarioCtx): Promise<void>;
}

function randomIndex(n: number): number {
  return Math.floor(Math.random() * n);
}

export const scenarios: Scenario[] = [
  {
    name: "get",
    run: async ({ doc, tableName, state }) => {
      const id = `id-${randomIndex(state.seedCount)}`;
      await doc.send(new GetCommand({ TableName: tableName, Key: { id } }));
    },
  },
  {
    name: "query-gsi",
    run: async ({ doc, tableName, state }) => {
      const board = randomIndex(state.boards);
      await doc.send(
        new QueryCommand({
          TableName: tableName,
          IndexName: "entity-comboKey-index",
          KeyConditionExpression: "entity = :e AND begins_with(comboKey, :prefix)",
          ExpressionAttributeValues: {
            ":e": "BACKLOG",
            ":prefix": comboKeyPrefix(subscriptionFor(board), board),
          },
          Limit: 100,
        }),
      );
    },
  },
  {
    name: "update-7gsi",
    run: async ({ doc, tableName, state }) => {
      const id = `id-${randomIndex(state.seedCount)}`;
      const changeStatus = Math.random() < 0.5;
      const update = changeStatus
        ? "SET #name = :n, #status = :s, updated = :t ADD version :one"
        : "SET #name = :n, updated = :t ADD version :one";
      const names = changeStatus ? { "#name": "name", "#status": "status" } : { "#name": "name" };
      const values: Record<string, unknown> = {
        ":n": `Updated ${id} ${state.nextName++}`,
        ":t": Date.now(),
        ":one": 1,
      };
      if (changeStatus) values[":s"] = STATUSES[randomIndex(STATUSES.length)];
      await doc.send(
        new UpdateCommand({
          TableName: tableName,
          Key: { id },
          UpdateExpression: update,
          ExpressionAttributeNames: names,
          ExpressionAttributeValues: values,
        }),
      );
    },
  },
  {
    name: "put",
    run: async ({ doc, tableName, state }) => {
      const id = `put-${state.nextPutId++}`;
      await doc.send(
        new PutCommand({
          TableName: tableName,
          Item: makePutItem(id, Date.now()),
          ConditionExpression: "attribute_not_exists(id)",
        }),
      );
    },
  },
];
