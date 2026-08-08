import { useEffect, useRef, useState } from 'react'
import { isApiError } from '../../../shared/api-client/ApiError'
import { Card } from '../../../shared/ui/Card'
import { Banner } from '../../../shared/ui/Banner'
import { Button } from '../../../shared/ui/Button'
import { Skeleton } from '../../../shared/ui/Skeleton'
import { formatMoney } from '../../accounts/money'
import { useOperationHistory } from '../useOperationHistory'
import type { OperationHistoryEntry } from '../api'
import styles from './OperationHistory.module.css'

const HIGHLIGHT_MS = 1500

function rowKey(entry: OperationHistoryEntry): string {
  return `${entry.type}-${entry.id}`
}

// Diffs each refetch's entries against the previous render's by
// updated_at, so a row that just transitioned (pending -> completed via
// the reconciliation worker, say) — or one that's brand new — flashes,
// while an untouched row sitting further down the list doesn't. React
// Query's default structural sharing means `entries` only gets a new
// array reference when something in it actually changed, so a fallback
// poll tick that returns identical data never trips this.
function useChangedRowKeys(entries: OperationHistoryEntry[] | undefined): Set<string> {
  const prevRef = useRef<Map<string, string> | null>(null)
  const [changed, setChanged] = useState<Set<string>>(new Set())
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    if (!entries) return
    const prev = prevRef.current
    const next = new Map(entries.map((entry) => [rowKey(entry), entry.updated_at]))
    if (prev) {
      const changedKeys = new Set<string>()
      for (const [key, updatedAt] of next) {
        if (prev.get(key) !== updatedAt) changedKeys.add(key)
      }
      if (changedKeys.size > 0) {
        setChanged(changedKeys)
        if (timerRef.current) clearTimeout(timerRef.current)
        timerRef.current = setTimeout(() => setChanged(new Set()), HIGHLIGHT_MS)
      }
    }
    prevRef.current = next
  }, [entries])

  useEffect(
    () => () => {
      if (timerRef.current) clearTimeout(timerRef.current)
    },
    [],
  )

  return changed
}

const TYPE_LABELS: Record<string, string> = {
  transfer: 'Перевод',
  deposit: 'Депозит',
  withdrawal: 'Вывод',
}

const STATUS_LABELS: Record<string, string> = {
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

// Money direction isn't a single shared field across types: transfers
// carry their own `direction` (outgoing/incoming), a deposit is always
// money coming in, and a (simulated) withdrawal is always money leaving —
// see services/transfers-svc/history.go's historyEntry doc comment for
// why those fields are only ever present on transfer entries.
function isOutgoing(entry: OperationHistoryEntry): boolean {
  if (entry.type === 'transfer') return entry.direction === 'outgoing'
  return entry.type === 'withdrawal'
}

export function OperationHistory() {
  const { data, isLoading, isError, error, refetch } = useOperationHistory()
  // Called unconditionally (before the loading/error early returns below)
  // per the rules of hooks.
  const changedKeys = useChangedRowKeys(data)

  if (isLoading) {
    return (
      <Card>
        <Skeleton className={styles.skeletonRow} />
        <Skeleton className={styles.skeletonRow} />
        <Skeleton className={styles.skeletonRow} />
      </Card>
    )
  }

  if (isError) {
    const message =
      isApiError(error) && error.status === 404
        ? 'Ваш счёт ещё создаётся — попробуйте обновить через несколько секунд.'
        : 'Не удалось загрузить историю операций.'
    return (
      <Card>
        <Banner variant="warning">{message}</Banner>
        <Button className={styles.retryButton} onClick={() => refetch()}>
          Повторить
        </Button>
      </Card>
    )
  }

  const entries = data!

  return (
    <Card>
      <h2>История операций</h2>
      {entries.length === 0 ? (
        <p className={styles.empty}>Операций пока нет.</p>
      ) : (
        <ul className={styles.list}>
          {entries.map((entry) => {
            const outgoing = isOutgoing(entry)
            const key = rowKey(entry)
            return (
              <li
                key={key}
                className={[styles.row, changedKeys.has(key) && styles.rowChanged].filter(Boolean).join(' ')}
              >
                <span className={styles.typeBadge}>
                  {TYPE_LABELS[entry.type] ?? entry.type}
                  {entry.type === 'withdrawal' && <span className={styles.simulationTag}> · симуляция</span>}
                </span>
                <span className={outgoing ? styles.amountOut : styles.amountIn}>
                  {outgoing ? '−' : '+'}
                  {formatMoney(entry.amount, 'EUR')}
                </span>
                <span className={styles.counterparty}>{entry.type === 'transfer' ? entry.counterparty_account_number : '—'}</span>
                <span className={[styles.status, entry.status === 'rejected' ? styles.rejected : ''].filter(Boolean).join(' ')}>
                  {STATUS_LABELS[entry.status] ?? entry.status}
                </span>
                <span className={styles.date}>{new Date(entry.created_at).toLocaleString('ru-RU')}</span>
              </li>
            )
          })}
        </ul>
      )}
    </Card>
  )
}
