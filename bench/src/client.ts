import { DynamoDBClient } from "@aws-sdk/client-dynamodb";
import { DynamoDBDocumentClient } from "@aws-sdk/lib-dynamodb";

// makeDocClient builds a DocumentClient. With no endpoint it
// targets the real service through the normal credential chain; with one it
// points at dynamoat (or DynamoDB Local).
export function makeDocClient(endpoint: string | undefined, region: string): DynamoDBDocumentClient {
  const client = new DynamoDBClient({
    region,
    // Against a local endpoint (dynamoat, DynamoDB Local) the SDK still signs
    // every request, so give it dummy credentials; dynamoat runs with
    // DYNAMOAT_AUTH=none in local development and does not check them.
    ...(endpoint
      ? { endpoint, credentials: { accessKeyId: "dynamoat", secretAccessKey: "dynamoat" } }
      : {}),
  });
  return DynamoDBDocumentClient.from(client);
}
