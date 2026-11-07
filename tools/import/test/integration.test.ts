import test from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import { promises as fs } from "node:fs";
import http from "node:http";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import {
  BatchWriteItemCommand,
  CreateTableCommand,
  DescribeTableCommand,
  DynamoDBClient,
  PutItemCommand,
  ScanCommand,
} from "@aws-sdk/client-dynamodb";
import { createClient } from "../src/clients";
import { DynamoTableReader } from "../src/source";
import { DynamoTableWriter } from "../src/target";
import { importTables } from "../src/import";
import { verifyTable } from "../src/verify";

const TABLE = "import-integration-test";

// The source client uses the normal SDK credential chain; even a local
// dynamoat with DYNAMOAT_AUTH=none needs something for the SDK to sign with.
process.env.AWS_ACCESS_KEY_ID = process.env.AWS_ACCESS_KEY_ID ?? "test";
process.env.AWS_SECRET_ACCESS_KEY = process.env.AWS_SECRET_ACCESS_KEY ?? "test";

const here = fileURLToPath(new URL(".", import.meta.url));
const importDir = path.resolve(here, "..");
const repoRoot = path.resolve(importDir, "..", "..");
const goScript = path.join(repoRoot, "scripts", "go.sh");
const serverDir = path.join(repoRoot, "server");

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.once("error", reject);
    srv.listen(0, "127.0.0.1", () => {
      const port = (srv.address() as net.AddressInfo).port;
      srv.close(() => resolve(port));
    });
  });
}

function startServer(bin: string, port: number): Promise<ChildProcess> {
  return new Promise((resolve, reject) => {
    const child = spawn(bin, [], {
      env: {
        ...process.env,
        DYNAMOAT_LISTEN: `127.0.0.1:${port}`,
        DYNAMOAT_AUTH: "none",
        DYNAMOAT_STORE: "memory",
        DYNAMOAT_LOG: "text",
      },
      stdio: ["ignore", "ignore", "pipe"],
    });
    let stderr = "";
    child.stderr?.on("data", (chunk: Buffer) => {
      stderr += chunk.toString();
    });
    const deadline = Date.now() + 15_000;
    const poll = (): void => {
      if (Date.now() > deadline) {
        child.kill();
        reject(new Error(`dynamoat did not become ready: ${stderr}`));
        return;
      }
      http
        .get(`http://127.0.0.1:${port}/readyz`, (res) => {
          res.resume();
          if (res.statusCode === 200) {
            resolve(child);
          } else {
            setTimeout(poll, 100);
          }
        })
        .on("error", () => setTimeout(poll, 100));
    };
    poll();
  });
}

test(
  "import copies a 3,000-item table with a GSI into dynamoat (--verify)",
  { timeout: 600_000 },
  async (t) => {
    const tmp = await fs.mkdtemp(path.join(os.tmpdir(), "dynamoat-import-"));
    const bin = path.join(tmp, "dynamoat");

    // Build the server with the repo's pinned toolchain.
    execFileSync("sh", [goScript, "-C", serverDir, "build", "-o", bin, "./cmd/dynamoat"], {
      cwd: importDir,
      stdio: "inherit",
    });

    const sourcePort = await freePort();
    const targetPort = await freePort();
    const sourceProc = await startServer(bin, sourcePort);
    const targetProc = await startServer(bin, targetPort);
    t.after(() => {
      sourceProc.kill();
      targetProc.kill();
      void fs.rm(tmp, { recursive: true, force: true });
    });

    const sourceEndpoint = `http://127.0.0.1:${sourcePort}`;
    const targetEndpoint = `http://127.0.0.1:${targetPort}`;

    const source = new DynamoDBClient({
      region: "us-east-1",
      endpoint: sourceEndpoint,
      credentials: { accessKeyId: "", secretAccessKey: "" },
    });
    const target = new DynamoDBClient({
      region: "us-east-1",
      endpoint: targetEndpoint,
      credentials: { accessKeyId: "", secretAccessKey: "" },
    });

    // The import needs Scan (source and verify) and BatchWriteItem (target).
    // Those are built by other cards; skip cleanly while they are missing.
    const missing = (
      await Promise.all([
        probe("Scan", () => source.send(new ScanCommand({ TableName: "probe-table" }))),
        probe(
          "BatchWriteItem",
          () => target.send(new BatchWriteItemCommand({ RequestItems: { "probe-table": [] } })),
        ),
      ])
    ).filter((name): name is string => name !== undefined);

    if (missing.length > 0) {
      t.skip(`dynamoat does not implement ${missing.join(", ")} yet; skipping the import integration test`);
      return;
    }

    // Fill the source with 3,000 items in a table that has a GSI.
    await source.send(
      new CreateTableCommand({
        TableName: TABLE,
        KeySchema: [
          { AttributeName: "pk", KeyType: "HASH" },
          { AttributeName: "sk", KeyType: "RANGE" },
        ],
        AttributeDefinitions: [
          { AttributeName: "pk", AttributeType: "S" },
          { AttributeName: "sk", AttributeType: "S" },
          { AttributeName: "gpk", AttributeType: "S" },
        ],
        BillingMode: "PAY_PER_REQUEST",
        GlobalSecondaryIndexes: [
          {
            IndexName: "gsi",
            KeySchema: [{ AttributeName: "gpk", KeyType: "HASH" }],
            Projection: { ProjectionType: "ALL" },
          },
        ],
      }),
    );

    for (let i = 0; i < 3000; i++) {
      await source.send(
        new PutItemCommand({
          TableName: TABLE,
          Item: {
            pk: { S: `p${i % 100}` },
            sk: { S: `s${String(i).padStart(4, "0")}` },
            gpk: { S: `g${i % 50}` },
            n: { N: String(i) },
            s: { S: `value-${i}` },
          },
        }),
      );
    }

    const reader = new DynamoTableReader(
      createClient({ region: "us-east-1", endpoint: sourceEndpoint }),
    );
    const writer = new DynamoTableWriter(
      createClient({
        region: "us-east-1",
        endpoint: targetEndpoint,
        credentials: { accessKeyId: "", secretAccessKey: "" },
      }),
    );

    const summary = await importTables({
      reader,
      writer,
      tables: [TABLE],
      segments: 8,
      statePath: path.join(tmp, "state.json"),
      verify: true,
      dryRun: false,
      progress: () => {},
    });

    assert.equal(summary.ok, true, JSON.stringify(summary, null, 2));
    assert.equal(summary.tables[0].verify?.match, true);

    // Independently check the target table: definition, GSI, counts, checksum.
    const desc = await target.send(new DescribeTableCommand({ TableName: TABLE }));
    assert.equal(desc.Table?.TableStatus, "ACTIVE");
    assert.ok(
      desc.Table?.GlobalSecondaryIndexes?.some((g) => g.IndexName === "gsi"),
      "target has the GSI",
    );

    const v = await verifyTable(reader, writer, TABLE);
    assert.equal(v.sourceCount, 3000);
    assert.equal(v.targetCount, 3000);
    assert.ok(v.match, `checksums differ: ${v.sourceChecksum} != ${v.targetChecksum}`);
  },
);

async function probe(name: string, fn: () => Promise<unknown>): Promise<string | undefined> {
  try {
    await fn();
  } catch (err) {
    if ((err as { name?: string }).name === "UnknownOperationException") {
      return name;
    }
  }
  return undefined;
}
