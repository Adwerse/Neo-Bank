// Profile 2 — HOT ACCOUNT: every transfer touches one particular account.
//
// What this is looking for, stated up front so the result is read
// correctly: ledger-svc's executeTransfer takes SELECT ... FOR UPDATE on
// both ledger accounts before checking the balance and inserting entries
// (services/ledger-svc/ledger.go). A row lock is held for the remainder of
// that transaction, so every transfer touching the hot row is serialized
// behind every other one. The hot row's throughput is therefore bounded by
// 1 / (time one transfer holds the lock), and adding VUs past that point
// buys queueing, not throughput.
//
// That is not a bug and this profile is not trying to expose one. It is
// the direct, unavoidable cost of enforcing "this account cannot go
// negative" at the row level, and the alternative designs (optimistic
// retry, per-account sharded balances, netting) all trade that guarantee
// or that simplicity away for it. The purpose here is to MEASURE the
// ceiling and be able to say what it is.
//
// HOT_DIRECTION picks which side of the transfer is hot:
//
//   inbound  (default) — everyone pays into one account. The cleanest
//                        experiment: contention is purely the recipient
//                        row lock, and every sender has its own separate
//                        fraud-velocity history, so nothing else is
//                        shared.
//   outbound           — one account pays everyone. Contention on the
//                        sender row PLUS a single fraud_checks partition
//                        that every velocity query now scans, plus a
//                        balance that actually drains. Slower than
//                        inbound, and the difference between the two is
//                        the cost of everything that is not the row lock.

import { scenarioOptions, transfer, users, runPrefix, runId } from './common.js';

export const options = scenarioOptions;
export { handleSummary } from './common.js';

const DIRECTION = __ENV.HOT_DIRECTION || 'inbound';
// users[0] is the hot account by convention — fixtures.json is ordered, so
// this is stable across runs and the verifier can be pointed at the same
// account without extra plumbing.
const HOT = 0;

export default function () {
  // Every other account takes the cold side, chosen at random so the cold
  // side never becomes a second, accidental hotspot.
  const other = 1 + Math.floor(Math.random() * (users.length - 1));
  const key = `${runPrefix}-hot-${runId}-${__VU}-${__ITER}`;

  if (DIRECTION === 'outbound') {
    transfer(users[HOT], users[other].iban, key);
  } else {
    transfer(users[other], users[HOT].iban, key);
  }
}
