import { isApiError } from '../../../shared/api-client/ApiError'
import { Badge } from '../../../shared/ui/Badge'
import { Card } from '../../../shared/ui/Card'
import { Banner } from '../../../shared/ui/Banner'
import { Button } from '../../../shared/ui/Button'
import { Money } from '../../../shared/ui/Money'
import { Skeleton } from '../../../shared/ui/Skeleton'
import { useChangedRowKeys } from '../../../shared/ui/useChangedRowKeys'
import { useOperationHistory } from '../useOperationHistory'
import { TYPE_LABELS, STATUS_LABELS, isOutgoing, isPosted, rowKey, statusBadgeVariant } from '../operationLabels'
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
            const posted = isPosted(entry)
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
                <Money
                  value={outgoing ? -entry.amount : entry.amount}
                  currency="EUR"
                  tone={posted ? 'auto' : 'pending'}
                  size="compact"
                />
                <span className={styles.counterparty}>{entry.type === 'transfer' ? entry.counterparty_iban : '—'}</span>
                <Badge variant={statusBadgeVariant(entry)}>{STATUS_LABELS[entry.status] ?? entry.status}</Badge>
                <span className={styles.date}>{new Date(entry.created_at).toLocaleString('ru-RU')}</span>
              </li>
            )
          })}
        </ul>
      )}
    </Card>
  )
}
