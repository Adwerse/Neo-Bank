import type { OperationHistoryEntry } from './api'

export const TYPE_LABELS: Record<string, string> = {
  transfer: 'Transfer',
  deposit: 'Deposit',
  withdrawal: 'Withdrawal',
}

export const STATUS_LABELS: Record<string, string> = {
  // transfer
  pending: 'Pending',
  completed: 'Completed',
  failed: 'Failed',
  rejected: 'Rejected',
  // deposit ('pending'/'failed' above already cover the shared cases)
  succeeded: 'Processing',
  credited: 'Credited',
  refunded: 'Refunded',
  // withdrawal
  payout_simulated: 'Simulated',
}

// Money direction isn't a single shared field across types: transfers carry
// their own `direction` (outgoing/incoming), a deposit is always money
// coming in, and a (simulated) withdrawal is always money leaving — see
// services/transfers-svc/history.go's historyEntry doc comment for why
// those fields are only ever present on transfer entries.
export function isOutgoing(entry: OperationHistoryEntry): boolean {
  if (entry.type === 'transfer') return entry.direction === 'outgoing'
  return entry.type === 'withdrawal'
}

// Whether this entry has actually moved money in the ledger yet — the only
// statuses computeRunningBalances/computeMoneyFlowBreakdown may account for.
// Everything else (pending, failed, rejected, refunded, a deposit still at
// 'succeeded' pre-credit) is real and shown, it just hasn't moved the
// balance, so it must not be summed as if it had.
export function isPosted(entry: OperationHistoryEntry): boolean {
  switch (entry.type) {
    case 'transfer':
      return entry.status === 'completed'
    case 'deposit':
      return entry.status === 'credited'
    case 'withdrawal':
      return entry.status === 'payout_simulated'
    default:
      return false
  }
}

export function rowKey(entry: OperationHistoryEntry): string {
  return `${entry.type}-${entry.id}`
}

export type StatusBadgeVariant = 'success' | 'warning' | 'danger' | 'pending'

// Exhaustive over every STATUS_LABELS key. 'succeeded' (a deposit Stripe
// confirmed but the ledger hasn't credited yet) maps to 'pending', not
// 'success' — see isPosted's doc comment for why that status isn't done
// yet. 'refunded' maps to 'warning': it's a reversal, not a failure, but
// also not the entry's original success outcome.
const STATUS_BADGE_VARIANT: Record<string, StatusBadgeVariant> = {
  pending: 'pending',
  completed: 'success',
  failed: 'danger',
  rejected: 'danger',
  succeeded: 'pending',
  credited: 'success',
  refunded: 'warning',
  payout_simulated: 'success',
}

export function statusBadgeVariant(entry: OperationHistoryEntry): StatusBadgeVariant {
  return STATUS_BADGE_VARIANT[entry.status] ?? 'pending'
}
