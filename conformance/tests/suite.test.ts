import { after, test } from 'node:test';
import { expectSame } from '../src/harness.ts';
import { stopTarget } from '../src/target.ts';
import { allCases } from './cases/index.ts';

for (const c of allCases) {
  test(c.name, async (t) => {
    await expectSame(c, t);
  });
}

after(async () => {
  await stopTarget();
});
