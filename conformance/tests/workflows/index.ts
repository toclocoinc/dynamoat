// A representative application's access patterns, one case per pattern.

import { batch } from './batch.ts';
import { cardEdit } from './card-edit.ts';
import { cardListing } from './card-listing.ts';
import { conditionalCreate } from './conditional-create.ts';
import { counters } from './counters.ts';
import { moneyTransfer } from './money-transfer.ts';
import { scan } from './scan.ts';
import { tenantTable } from './tenant-table.ts';
import type { Case } from '../cases/shared.ts';

export const appCases: Case[] = [
  tenantTable,
  cardListing,
  conditionalCreate,
  counters,
  cardEdit,
  batch,
  moneyTransfer,
  scan,
];
