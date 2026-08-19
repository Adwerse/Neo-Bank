// Profile 3 — DUPLICATES: many concurrent requests carrying the SAME
// idempotency key.
//
// The unit test for idempotency proves the logic is right when two calls
// are made in sequence, or when a handful race. What it cannot show is
// whether the protection survives sustained concurrency, where the losing
// side of the race is not a hypothetical: transfers-svc checks for an
// existing transfer first (a fast path that two requests can BOTH pass),
// and then relies on the UNIQUE constraint on idempotency_key as the real
// arbiter, catching the unique violation and returning the winner's row.
// This profile keeps that second path hot for minutes at a time.
//
// The failure mode being hunted is specific and would not show up as an
// error in these results at all: two requests with one key both reaching
// ledger-svc, producing one transfers row and two postings. Every HTTP
// response would look perfectly fine. It is caught after the run, by
// `lt verify`'s transfer_entries_paired check — a double-post appears as
// four entries against a transfer instead of two. That is why this profile
// is meaningless without the verification step, more so than the other
// two.
//
// FANOUT is how many requests share one key. Iterations are grouped by
// k6's global iteration counter, so a group's requests are issued by
// different VUs within a few milliseconds of each other — which is the
// point. Sender, recipient and amount are derived from the group index
// rather than chosen randomly, because they have to be IDENTICAL across a
// group: transfers-svc rejects a key reused with different parameters
// (422, reconcileReplay), and that would test the wrong branch.

import exec from 'k6/execution';
import { scenarioOptions, transfer, users, runPrefix, runId } from './common.js';

export const options = scenarioOptions;
export { handleSummary } from './common.js';

const FANOUT = Number(__ENV.FANOUT || 10);

export default function () {
  const group = Math.floor(exec.scenario.iterationInTest / FANOUT);
  const from = group % users.length;
  const to = (group + 1) % users.length;
  const key = `${runPrefix}-dup-${runId}-${group}`;
  transfer(users[from], users[to].iban, key);
}
