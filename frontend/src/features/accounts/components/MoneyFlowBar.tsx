import { computeMoneyFlowBreakdown, type MoneyFlowCategory } from '../moneyFlow'
import { formatMoney } from '../money'
import type { OperationHistoryEntry } from '../../transfers/api'
import styles from './MoneyFlowBar.module.css'

const CATEGORY_COLOR_VAR: Record<MoneyFlowCategory, string> = {
  'in-transfer': 'var(--money-in)',
  'out-transfer': 'var(--money-out)',
  deposit: 'var(--color-accent)',
  withdrawal: 'var(--color-accent-2)',
}

interface MoneyFlowBarProps {
  entries: OperationHistoryEntry[]
}

export function MoneyFlowBar({ entries }: MoneyFlowBarProps) {
  const buckets = computeMoneyFlowBreakdown(entries)

  if (buckets.length === 0) {
    return <p className={styles.empty}>Операций пока нет.</p>
  }

  return (
    <div>
      <div className={styles.bar}>
        {buckets.map((bucket) => (
          <div
            key={bucket.category}
            className={styles.segment}
            style={{ flex: bucket.pct, background: CATEGORY_COLOR_VAR[bucket.category] }}
          />
        ))}
      </div>
      <div className={styles.legend}>
        {buckets.map((bucket) => (
          <div key={bucket.category} className={styles.legendItem}>
            <span className={styles.dot} style={{ background: CATEGORY_COLOR_VAR[bucket.category] }} />
            <span>{bucket.label}</span>
            <span className={styles.amount}>{formatMoney(bucket.amountMinorUnits, 'EUR')}</span>
          </div>
        ))}
      </div>
    </div>
  )
}
