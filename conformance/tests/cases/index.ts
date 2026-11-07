// Every test case, in run order. list-tables runs first so the dynamoat
// target's table list contains only its own two tables.

import { controlPlane } from './control-plane.ts';
import { backups } from './backups.ts';
import { appCases } from '../workflows/index.ts';
import { errors } from './errors.ts';
import { globalTables } from './global-tables.ts';
import { itemOps } from './item-ops.ts';
import { partiqlCases } from './partiql.ts';
import { pitr } from './pitr.ts';
import { streamsCases } from './streams.ts';
import type { Case } from './shared.ts';
import { tags } from './tags.ts';
import { ttl } from './ttl.ts';
import { unmerged } from './unmerged.ts';

export const allCases: Case[] = [
  ...controlPlane,
  ...backups,
  ...globalTables,
  ...tags,
  ...ttl,
  ...pitr,
  ...itemOps,
  ...partiqlCases,
  ...streamsCases,
  ...unmerged,
  ...errors,
  ...appCases,
];
