import { CopyButton } from '../../../shared/ui/CopyButton'
import { Money } from '../../../shared/ui/Money'
import { Skeleton } from '../../../shared/ui/Skeleton'
import { Tag } from '../../../shared/ui/Tag'
import { useFlashOnChange } from '../../../shared/ui/useFlashOnChange'
import type { MeResponse } from '../api'
import { computeBalanceDelta } from '../runningBalance'
import { ACCOUNT_STATUS_LABELS } from '../statusLabels'
import type { useDashboardOperations } from '../useDashboardOperations'
import { BalanceChart } from './BalanceChart'
import { MoneyFlowBar } from './MoneyFlowBar'
import { QuickActions } from './QuickActions'
import { TerminalOperationList } from './TerminalOperationList'
import styles from './TerminalDashboard.module.css'

interface TerminalDashboardProps {
  // undefined while useMe() is still loading — each block below skeletons
  // its own piece of this rather than the whole page waiting on one spinner.
  account: MeResponse | undefined
  operations: ReturnType<typeof useDashboardOperations>
}

// Avatar/greeting/bell and LiveIndicator live in MobileShell now — that
// chrome is the same across every mobile page, not just the dashboard, the
// same way DesktopShell's header isn't BlueprintDashboard's to own either.
export function TerminalDashboard({ account, operations }: TerminalDashboardProps) {
  const { entries, balanceBeforeEarliest, isLoading, isError, refetch } = operations

  const earliestCreatedAt = entries[entries.length - 1]?.created_at
  const delta = account ? computeBalanceDelta(account.balance, balanceBeforeEarliest, earliestCreatedAt) : null

  return (
    <div className={styles.screen}>
      <div>
        <div className={styles.balanceLabel}>Баланс</div>
        <div className={styles.balanceValueRow}>
          {account ? <TerminalBalanceValue account={account} /> : <Skeleton className={styles.balanceSkeleton} />}
        </div>
        {account && delta && delta.periodLabel && (
          <div className={delta.direction === 'down' ? styles.deltaDown : styles.deltaUp}>
            <span>{delta.direction === 'down' ? '▼' : '▲'}</span>
            {delta.pct !== null ? (
              `${delta.pct >= 0 ? '+' : ''}${delta.pct.toFixed(1)}%`
            ) : (
              <Money value={delta.absoluteMinorUnits} currency={account.currency} size="compact" />
            )}{' '}
            за {delta.periodLabel}
          </div>
        )}
      </div>

      <div className={styles.section}>
        <BalanceChart account={account} header={<span className={styles.sectionLabel}>Баланс во времени</span>} />
      </div>

      <div id="account-details" className={styles.identityChips}>
        {account ? (
          <>
            <Tag variant="accent">{ACCOUNT_STATUS_LABELS[account.status] ?? account.status}</Tag>
            <span className={styles.iban}>{account.iban}</span>
            <CopyButton value={account.iban} label="Скопировать IBAN" />
          </>
        ) : (
          <Skeleton className={styles.ibanSkeleton} />
        )}
      </div>

      <QuickActions layout="row" />

      <div className={styles.section}>
        <div className={styles.sectionLabel}>Движение средств</div>
        <MoneyFlowBar entries={entries} />
      </div>

      <TerminalOperationList entries={entries} isLoading={isLoading} isError={isError} onRetry={refetch} />
    </div>
  )
}

// Split out so useFlashOnChange only mounts once account is actually
// loaded — otherwise the undefined-to-first-value transition on initial
// load would itself register as a "changed" balance and flash on mount.
function TerminalBalanceValue({ account }: { account: MeResponse }) {
  const balanceFlash = useFlashOnChange(account.balance)
  return (
    <Money
      value={account.balance}
      currency={account.currency}
      showSign={false}
      size="hero"
      label="Баланс"
      className={balanceFlash ? styles.balanceFlash : undefined}
    />
  )
}
