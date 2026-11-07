// Tag cases: TagResource, ListTagsOfResource and UntagResource, sent through
// the base DynamoDBClient that the DocumentClient wraps (tag commands are not
// re-exported by lib-dynamodb).

import { DescribeTableCommand, ListTagsOfResourceCommand, TagResourceCommand, UntagResourceCommand } from '@aws-sdk/client-dynamodb';
import { callRetrying, captureWhenReady } from '../../src/harness.ts';
import { deep } from '../assert.ts';
import { simpleTable } from './shared.ts';
import type { Case } from './shared.ts';

const NAME = 'tags';

// tagsContain reports whether every expected (Key, Value) tag is present, so
// the harness can poll ListTagsOfResource until TagResource/UntagResource take
// effect (they are eventually consistent on real DynamoDB).
function tagsContain(res: unknown, expected: { Key: string; Value: string }[]): boolean {
  const tags = (res as { Tags?: { Key?: string; Value?: string }[] } | null)?.Tags ?? [];
  return expected.every((want) => tags.some((t) => t.Key === want.Key && t.Value === want.Value));
}

const TAG_RETRY = ['ResourceInUseException', 'LimitExceededException'];

function byKey(tags: { Key?: string; Value?: string }[]): { Key?: string; Value?: string }[] {
  return [...tags].sort((a, b) => String(a.Key).localeCompare(String(b.Key)));
}

export const tags: Case[] = [
  {
    name: 'tags',
    requires: ['ListTagsOfResource', 'TagResource', 'UntagResource'],
    slow: true,
    fn: async (ctx) => {
      const table = ctx.table('tags');
      await ctx.createTable(simpleTable(table));

      const desc = (await ctx.call(() => ctx.ddb.send(new DescribeTableCommand({ TableName: table })))) as {
        Table?: { TableArn?: string };
      };
      const arn = desc.Table?.TableArn;
      if (!arn) throw new Error('DescribeTable returned no TableArn');

      const expected = [
        { Key: 'env', Value: 'prod' },
        { Key: 'owner', Value: 'platform' },
      ];

      // Real DynamoDB rejects a tag operation while the previous one on the
      // same resource is still being applied (ResourceInUseException).
      await callRetrying(
        ctx,
        'TagResource',
        () => ctx.ddb.send(new TagResourceCommand({ ResourceArn: arn, Tags: expected })),
        TAG_RETRY,
      );

      const listed = (await captureWhenReady(
        ctx,
        'ListTagsOfResource',
        () => ctx.ddb.send(new ListTagsOfResourceCommand({ ResourceArn: arn })),
        (res) => tagsContain(res, expected) && (res as { Tags?: unknown[] }).Tags?.length === expected.length,
      )) as { Tags?: { Key: string; Value: string }[] };
      // DynamoDB does not define the order of ListTagsOfResource (recorded: not
      // key order), so compare as a set.
      deep(NAME, byKey(listed.Tags ?? []), byKey(expected), 'ListTagsOfResource returns the tags');

      await callRetrying(
        ctx,
        'UntagResource',
        () => ctx.ddb.send(new UntagResourceCommand({ ResourceArn: arn, TagKeys: ['env'] })),
        TAG_RETRY,
      );

      const after = (await captureWhenReady(
        ctx,
        'ListTagsOfResource after untag',
        () => ctx.ddb.send(new ListTagsOfResourceCommand({ ResourceArn: arn })),
        (res) => tagsContain(res, [{ Key: 'owner', Value: 'platform' }]) && (res as { Tags?: unknown[] }).Tags?.length === 1,
      )) as { Tags?: { Key: string; Value: string }[] };
      deep(NAME, after.Tags, [{ Key: 'owner', Value: 'platform' }], 'UntagResource removed the tag');
    },
  },
];
