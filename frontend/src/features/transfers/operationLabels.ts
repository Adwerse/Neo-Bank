import type { OperationHistoryEntry } from './api'

export const TYPE_LABELS: Record<string, string> = {
  transfer: 'Перевод',
  deposit: 'Депозит',
  withdrawal: 'Вывод',
}

export const STATUS_LABELS: Record<string, string> = {
  // transfer
  pending: 'В обработке',
  completed: 'Выполнен',
  failed: 'Не выполнен',
  rejected: 'Заблокирован',
  // deposit ('pending'/'failed' above already cover the shared cases)
  succeeded: 'Обрабатывается банком',
  credited: 'Зачислено',
  refunded: 'Возвращено',
  // withdrawal
  payout_simulated: 'Симуляция выполнена',
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
