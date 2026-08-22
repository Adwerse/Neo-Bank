import { isOutgoing, isPosted } from '../transfers/operationLabels'
import { formatDurationSince } from '../transfers/relativeTime'
import type { OperationHistoryEntry } from '../transfers/api'

export interface AnnotatedOperation extends OperationHistoryEntry {
  // The ledger balance immediately after this entry, in minor units. For a
  // posted entry this is the real post-transaction balance; for one that
  // hasn't posted yet (pending/failed/rejected/refunded/pre-credit) it's
  // just "whatever the balance already was" — shown, not faked, but also
  // not treated as having moved anything.
  balanceAfter: number
}

export interface RunningBalanceResult {
  entries: AnnotatedOperation[]
  // One step further back than the oldest fetched entry — the balance as it
  // stood right before that entry. This is what makes the delta/chart math
  // correct without a second pass, and it's honestly scoped to "before the
  // oldest entry we actually fetched," not a claim about account history.
  balanceBeforeEarliest: number
}

// GET /accounts/me gives the current, live balance. GET /transfers gives the
// last N raw operations, newest-first. There is no running-balance field and
// no history endpoint anywhere in the API, so this is the one place that
// derives "balance after each row" — by walking backward from the current
// balance and only undoing entries that actually posted to the ledger.
export function computeRunningBalances(
  currentBalanceMinorUnits: number,
  entries: OperationHistoryEntry[],
): RunningBalanceResult {
  const annotated: AnnotatedOperation[] = []
  let running = currentBalanceMinorUnits
  for (const entry of entries) {
    annotated.push({ ...entry, balanceAfter: running })
    if (isPosted(entry)) {
      const delta = isOutgoing(entry) ? -entry.amount : entry.amount
      running -= delta
    }
  }
  return { entries: annotated, balanceBeforeEarliest: running }
}

export interface BalanceDelta {
  // null (not Infinity/NaN) when balanceBeforeEarliest was 0 — there's no
  // meaningful percentage change from a zero base, so callers must fall
  // back to showing only the absolute amount in that case.
  pct: number | null
  absoluteMinorUnits: number
  // A real span of whatever was actually fetched ("12 дней"), never a
  // hardcoded period like the source mockup's illustrative "6 мес".
  periodLabel: string
  direction: 'up' | 'down' | 'flat'
}

export function computeBalanceDelta(
  currentBalanceMinorUnits: number,
  balanceBeforeEarliest: number,
  earliestEntryCreatedAt: string | undefined,
): BalanceDelta {
  const absoluteMinorUnits = currentBalanceMinorUnits - balanceBeforeEarliest
  const pct = balanceBeforeEarliest === 0 ? null : (absoluteMinorUnits / Math.abs(balanceBeforeEarliest)) * 100
  const direction: BalanceDelta['direction'] = absoluteMinorUnits > 0 ? 'up' : absoluteMinorUnits < 0 ? 'down' : 'flat'
  const periodLabel = earliestEntryCreatedAt ? formatDurationSince(earliestEntryCreatedAt) : ''
  return { pct, absoluteMinorUnits, periodLabel, direction }
}
