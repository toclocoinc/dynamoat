import { DynamoDBClient } from "@aws-sdk/client-dynamodb";

export interface ClientConfig {
  endpoint?: string;
  region: string;
  credentials?: { accessKeyId: string; secretAccessKey: string };
}

// normalizeEndpoint accepts a bare host:port (DynamoDB Local style) and adds
// the http scheme the SDK requires.
export function normalizeEndpoint(url: string): string {
  if (!/^[a-zA-Z][a-zA-Z0-9+.-]*:\/\//.test(url)) {
    return `http://${url}`;
  }
  return url;
}

// createClient builds a DynamoDB client. When credentials is omitted the
// client uses the normal SDK credential chain; when it is set (even to empty
// strings) the client signs with exactly those credentials and never falls
// back to the ambient environment.
export function createClient(config: ClientConfig): DynamoDBClient {
  return new DynamoDBClient({
    region: config.region,
    ...(config.endpoint !== undefined ? { endpoint: normalizeEndpoint(config.endpoint) } : {}),
    ...(config.credentials !== undefined ? { credentials: config.credentials } : {}),
  });
}
