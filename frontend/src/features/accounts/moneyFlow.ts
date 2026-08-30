import { isPosted } from '../transfers/operationLabels'
import type { OperationHistoryEntry } from '../transfers/api'

export type MoneyFlowCategory = 'in-transfer' | 'out-transfer' | 'deposit' | 'withdrawal'

export interface MoneyFlowBucket {
  category: MoneyFlowCategory
  label: string
  amountMinorUnits: number
  pct: number
}

const CATEGORY_ORDER: MoneyFlowCategory[] = ['in-transfer', 'deposit', 'out-transfer', 'withdrawal']

const CATEGORY_LABELS: Record<MoneyFlowCategory, string> = {
  'in-transfer': 'Incoming transfers',
  deposit: 'Deposits',
  'out-transfer': 'Outgoing transfers',
  withdrawal: 'Withdrawals',
}

function categorize(entry: OperationHistoryEntry): MoneyFlowCategory {
  if (entry.type === 'deposit') return 'deposit'
  if (entry.type === 'withdrawal') return 'withdrawal'
  return entry.direction === 'outgoing' ? 'out-transfer' : 'in-transfer'
}

// Buckets the same fetched operations page used everywhere else on the
// dashboard — only posted entries count, same rule as computeRunningBalances
// (an entry that hasn't moved money yet shouldn't be counted as flow).
// Empty buckets are omitted so the bar never renders a 0%-width segment.
export function computeMoneyFlowBreakdown(entries: OperationHistoryEntry[]): MoneyFlowBucket[] {
  const sums: Record<MoneyFlowCategory, number> = {
    'in-transfer': 0,
    'out-transfer': 0,
    deposit: 0,
    withdrawal: 0,
  }
  for (const entry of entries) {
    if (!isPosted(entry)) continue
    sums[categorize(entry)] += Math.abs(entry.amount)
  }
  const total = CATEGORY_ORDER.reduce((sum, category) => sum + sums[category], 0)
  if (total === 0) return []
  return CATEGORY_ORDER.filter((category) => sums[category] > 0).map((category) => ({
    category,
    label: CATEGORY_LABELS[category],
    amountMinorUnits: sums[category],
    pct: (sums[category] / total) * 100,
  }))
}
