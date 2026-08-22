import { Banner } from '../../../shared/ui/Banner'
import { Button } from '../../../shared/ui/Button'
import { Card } from '../../../shared/ui/Card'
import { Skeleton } from '../../../shared/ui/Skeleton'
import { useIsDesktop } from '../../../shared/ui/useIsDesktop'
import { useScrollToHash } from '../../../shared/ui/useScrollToHash'
import { getAccountErrorMessage } from '../errorMessages'
import { useDashboardOperations } from '../useDashboardOperations'
import { useMe } from '../useMe'
import { BlueprintDashboard } from './BlueprintDashboard'
import styles from './DashboardPage.module.css'
import { TerminalDashboard } from './TerminalDashboard'

export function DashboardPage() {
  const { data, isLoading, isError, error, refetch } = useMe()
  const isDesktop = useIsDesktop()
  // Called unconditionally (before the loading/error early returns below)
  // per the rules of hooks — undefined/empty results until data resolves
  // are handled internally, that's not a "change" either.
  const operations = useDashboardOperations(data?.balance)
  useScrollToHash()

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
    return (
      <Card>
        <Banner variant="warning">{getAccountErrorMessage(error)}</Banner>
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
    <div className={styles.container}>
      {isRestricted && (
        <Banner variant={account.status === 'closed' ? 'danger' : 'warning'} className={styles.statusBanner}>
          {account.status === 'closed'
            ? 'Счёт закрыт. Операции недоступны — обратитесь в поддержку.'
            : 'Счёт заморожен. Операции временно недоступны.'}
        </Banner>
      )}
      {isDesktop ? (
        <BlueprintDashboard account={account} operations={operations} />
      ) : (
        <TerminalDashboard account={account} operations={operations} />
      )}
    </div>
  )
}
