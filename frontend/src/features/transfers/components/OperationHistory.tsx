import { isApiError } from '../../../shared/api-client/ApiError'
import { Card } from '../../../shared/ui/Card'
import { Banner } from '../../../shared/ui/Banner'
import { Button } from '../../../shared/ui/Button'
import { Skeleton } from '../../../shared/ui/Skeleton'
import { useChangedRowKeys } from '../../../shared/ui/useChangedRowKeys'
import { formatMoney } from '../../accounts/money'
import { useOperationHistory } from '../useOperationHistory'
import { TYPE_LABELS, STATUS_LABELS, isOutgoing, rowKey } from '../operationLabels'
import styles from './OperationHistory.module.css'

export function OperationHistory() {
  const { data, isLoading, isError, error, refetch } = useOperationHistory()
  // Called unconditionally (before the loading/error early returns below)
  // per the rules of hooks.
  const changedKeys = useChangedRowKeys(data, rowKey, (entry) => entry.updated_at)

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
                <span className={styles.counterparty}>{entry.type === 'transfer' ? entry.counterparty_iban : '—'}</span>
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
