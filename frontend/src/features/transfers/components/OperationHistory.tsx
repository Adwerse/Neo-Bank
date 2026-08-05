import { isApiError } from '../../../shared/api-client/ApiError'
import { Card } from '../../../shared/ui/Card'
import { Banner } from '../../../shared/ui/Banner'
import { Button } from '../../../shared/ui/Button'
import { Skeleton } from '../../../shared/ui/Skeleton'
import { formatMoney } from '../../accounts/money'
import { useOperationHistory } from '../useOperationHistory'
import type { OperationHistoryEntry } from '../api'
import styles from './OperationHistory.module.css'

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
            return (
              <li key={`${entry.type}-${entry.id}`} className={styles.row}>
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
