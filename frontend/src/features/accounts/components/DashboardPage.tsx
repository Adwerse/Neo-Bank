import { isApiError } from '../../../shared/api-client/ApiError'
import { getAccessTokenEmail } from '../../../shared/api-client/jwt'
import { Card } from '../../../shared/ui/Card'
import { Banner } from '../../../shared/ui/Banner'
import { Button } from '../../../shared/ui/Button'
import { Skeleton } from '../../../shared/ui/Skeleton'
import { useMe } from '../useMe'
import { formatMoney } from '../money'
import styles from './DashboardPage.module.css'

const STATUS_LABELS: Record<string, string> = {
  active: 'Активен',
  frozen: 'Заморожен',
  closed: 'Закрыт',
}

export function DashboardPage() {
  const { data, isLoading, isError, error, refetch } = useMe()
  const email = getAccessTokenEmail()

  if (isLoading) {
    return (
      <Card>
        <Skeleton className={styles.skeletonBalance} />
        <Skeleton className={styles.skeletonRow} />
        <Skeleton className={styles.skeletonRow} />
        <Skeleton className={styles.skeletonRow} />
      </Card>
    )
  }

  if (isError) {
    // Never fall back to showing a 0 balance here — an honest "unavailable"
    // beats a fake number in a banking product.
    let message = 'Не удалось загрузить данные счёта, попробуйте ещё раз.'
    if (isApiError(error) && error.status === 503) {
      message = 'Баланс временно недоступен. Попробуйте ещё раз через минуту.'
    } else if (isApiError(error) && error.status === 404) {
      message = 'Ваш счёт ещё создаётся — это обычно занимает несколько секунд. Попробуйте обновить.'
    }
    return (
      <Card>
        <Banner variant="warning">{message}</Banner>
        <Button className={styles.retryButton} onClick={() => refetch()}>
          Повторить
        </Button>
      </Card>
    )
  }

  // Safe: the loading/error branches above already returned, so this is
  // always the success case — react-query's isLoading/isError booleans just
  // don't narrow `data`'s type once destructured.
  const account = data!
  const isRestricted = account.status === 'frozen' || account.status === 'closed'

  return (
    <Card>
      {isRestricted && (
        <Banner variant={account.status === 'closed' ? 'danger' : 'warning'} className={styles.statusBanner}>
          {account.status === 'closed'
            ? 'Счёт закрыт. Операции недоступны — обратитесь в поддержку.'
            : 'Счёт заморожен. Операции временно недоступны.'}
        </Banner>
      )}

      <div className={styles.balance}>{formatMoney(account.balance, account.currency)}</div>
      {account.balance === 0 && (
        <p className={styles.hint}>Пополнение появится позже — принимаем платежи через Stripe со спринта 9.</p>
      )}

      <dl className={styles.details}>
        <dt className={styles.label}>Номер счёта</dt>
        <dd className={styles.value}>{account.account_number}</dd>

        <dt className={styles.label}>Статус</dt>
        <dd className={styles.value}>{STATUS_LABELS[account.status] ?? account.status}</dd>

        <dt className={styles.label}>Email</dt>
        <dd className={styles.value}>{email ?? '—'}</dd>
      </dl>
    </Card>
  )
}
