// Profile 1 — DISTRIBUTED: N users transfer to each other at random.
//
// This is the baseline. Contention is spread across the whole cohort, so
// on any given transfer the odds of colliding with another transfer on
// either of its two ledger rows are low. Whatever ceiling this profile
// finds is therefore a ceiling of the PIPELINE — gateway hops, the fraud
// query, the number of synchronously-replicated commits per transfer —
// and not of lock contention. That is what makes it the control the other
// two profiles are read against.

import { scenarioOptions, transfer, users, pickTwo, runPrefix, runId } from './common.js';

export const options = scenarioOptions;
export { handleSummary } from './common.js';

export default function () {
  const [from, to] = pickTwo(users.length);
  // Unique per request: __VU and __ITER identify the iteration within this
  // invocation, runId separates invocations. A collision here would send
  // the request down the replay path and quietly stop measuring the thing
  // this profile exists to measure.
  const key = `${runPrefix}-dist-${runId}-${__VU}-${__ITER}`;
  transfer(users[from], users[to].iban, key);
}
