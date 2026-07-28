import { isApiError } from '../../../shared/api-client/ApiError'
import { Card } from '../../../shared/ui/Card'
import { Banner } from '../../../shared/ui/Banner'
import { Button } from '../../../shared/ui/Button'
import { Skeleton } from '../../../shared/ui/Skeleton'
import { formatMoney } from '../../accounts/money'
import { useTransferHistory } from '../useTransferHistory'
import styles from './TransferHistory.module.css'

const STATUS_LABELS: Record<string, string> = {
  pending: 'В обработке',
  completed: 'Выполнен',
  failed: 'Не выполнен',
  rejected: 'Заблокирован',
}

export function TransferHistory() {
  const { data, isLoading, isError, error, refetch } = useTransferHistory()

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
        : 'Не удалось загрузить историю переводов.'
    return (
      <Card>
        <Banner variant="warning">{message}</Banner>
        <Button className={styles.retryButton} onClick={() => refetch()}>
          Повторить
        </Button>
      </Card>
    )
  }

  const transfers = data!

  return (
    <Card>
      <h2>История переводов</h2>
      {transfers.length === 0 ? (
        <p className={styles.empty}>Переводов пока нет.</p>
      ) : (
        <ul className={styles.list}>
          {transfers.map((t) => (
            <li key={t.id} className={styles.row}>
              <span className={t.direction === 'sent' ? styles.amountSent : styles.amountReceived}>
                {t.direction === 'sent' ? '−' : '+'}
                {formatMoney(t.amount, 'EUR')}
              </span>
              <span className={styles.counterparty}>{t.counterparty_account_number}</span>
              <span className={[styles.status, t.status === 'rejected' ? styles.rejected : ''].filter(Boolean).join(' ')}>
                {STATUS_LABELS[t.status] ?? t.status}
              </span>
              <span className={styles.date}>{new Date(t.created_at).toLocaleString('ru-RU')}</span>
            </li>
          ))}
        </ul>
      )}
    </Card>
  )
}
