import { Banner } from '../../../shared/ui/Banner'
import { Button } from '../../../shared/ui/Button'
import { Card } from '../../../shared/ui/Card'
import { useIsDesktop } from '../../../shared/ui/useIsDesktop'
import { useScrollToHash } from '../../../shared/ui/useScrollToHash'
import { getAccountErrorMessage } from '../errorMessages'
import { useDashboardOperations } from '../useDashboardOperations'
import { useMe } from '../useMe'
import { BlueprintDashboard } from './BlueprintDashboard'
import styles from './DashboardPage.module.css'
import { TerminalDashboard } from './TerminalDashboard'

export function DashboardPage() {
  const { data, isError, error, refetch } = useMe()
  const isDesktop = useIsDesktop()
  // Called unconditionally (before the isError early return below) per the
  // rules of hooks — undefined/empty results until data resolves are
  // handled internally, that's not a "change" either.
  const operations = useDashboardOperations(data?.balance)
  useScrollToHash()

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

  // undefined while still loading — BlueprintDashboard/TerminalDashboard
  // render their fixed block layout regardless, each block skeletoning its
  // own piece off this instead of the whole page waiting on one spinner.
  const account = data
  const isRestricted = account?.status === 'frozen' || account?.status === 'closed'

  return (
    <div className={styles.container}>
      {account && isRestricted && (
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
