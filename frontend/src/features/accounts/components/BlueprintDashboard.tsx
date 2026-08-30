import { CopyButton } from '../../../shared/ui/CopyButton'
import { CornerBracketPanel } from '../../../shared/ui/CornerBracketPanel'
import { Money } from '../../../shared/ui/Money'
import { NumberedBadge } from '../../../shared/ui/NumberedBadge'
import { Skeleton } from '../../../shared/ui/Skeleton'
import { useFlashOnChange } from '../../../shared/ui/useFlashOnChange'
import { APP_LOCALE } from '../../../shared/locale'
import type { MeResponse } from '../api'
import { computeBalanceDelta } from '../runningBalance'
import { ACCOUNT_STATUS_LABELS } from '../statusLabels'
import type { useDashboardOperations } from '../useDashboardOperations'
import { BalanceChart } from './BalanceChart'
import { BlueprintOperationTable } from './BlueprintOperationTable'
import { MoneyFlowBar } from './MoneyFlowBar'
import { QuickActions } from './QuickActions'
import styles from './BlueprintDashboard.module.css'

interface BlueprintDashboardProps {
  // undefined while useMe() is still loading — each block below skeletons
  // its own piece of this rather than the whole page waiting on one spinner.
  account: MeResponse | undefined
  operations: ReturnType<typeof useDashboardOperations>
}

const STATUS_COLOR_CLASS: Record<string, string> = {
  active: 'statusActive',
  frozen: 'statusFrozen',
  closed: 'statusClosed',
}

const openedDateFormat = new Intl.DateTimeFormat(APP_LOCALE)

export function BlueprintDashboard({ account, operations }: BlueprintDashboardProps) {
  const { entries, balanceBeforeEarliest, isLoading, isError, refetch } = operations
  const earliestCreatedAt = entries[entries.length - 1]?.created_at
  const delta = account ? computeBalanceDelta(account.balance, balanceBeforeEarliest, earliestCreatedAt) : null
  const statusClass = account ? styles[STATUS_COLOR_CLASS[account.status] ?? 'statusClosed'] : undefined

  return (
    <div className={styles.page}>
      <div className={styles.infoStrip}>
        <div className={styles.infoCell}>
          <div className={styles.infoLabel}>Account</div>
          <div className={styles.infoValue}>
            {account ? account.account_number : <Skeleton className={styles.infoValueSkeleton} />}
          </div>
        </div>
        <div className={styles.infoCell}>
          <div className={styles.infoLabel}>Currency</div>
          <div className={styles.infoValue}>{account ? account.currency : <Skeleton className={styles.infoValueSkeleton} />}</div>
        </div>
        <div className={styles.infoCell}>
          <div className={styles.infoLabel}>Status</div>
          <div className={[styles.infoValue, statusClass].filter(Boolean).join(' ')}>
            {account ? (ACCOUNT_STATUS_LABELS[account.status] ?? account.status) : <Skeleton className={styles.infoValueSkeleton} />}
          </div>
        </div>
        <div className={styles.infoCell}>
          <div className={styles.infoLabel}>Type</div>
          {/* Every account in this system is the same single product — a
              true constant, not a fabricated per-account field. */}
          <div className={styles.infoValue}>Current</div>
        </div>
        <div className={styles.infoCellLast}>
          <div className={styles.infoLabel}>Opened</div>
          <div className={styles.infoValue}>
            {account ? openedDateFormat.format(new Date(account.created_at)) : <Skeleton className={styles.infoValueSkeleton} />}
          </div>
        </div>
      </div>

      <CornerBracketPanel>
        <div className={styles.balancePanel}>
          <NumberedBadge n={1} label="Balance" />
          <div className={styles.balanceValueRow}>
            {account ? <BlueprintBalanceValue account={account} /> : <Skeleton className={styles.balanceSkeleton} />}
          </div>
          <svg className={styles.dimensionLine} viewBox="0 0 220 14" preserveAspectRatio="none" aria-hidden="true">
            <line x1="1" y1="7" x2="219" y2="7" stroke="var(--color-accent)" strokeWidth="1" />
            <line x1="1" y1="2" x2="1" y2="12" stroke="var(--color-accent)" strokeWidth="1" />
            <line x1="219" y1="2" x2="219" y2="12" stroke="var(--color-accent)" strokeWidth="1" />
          </svg>
          {account && delta && delta.periodLabel && (
            <div className={delta.direction === 'down' ? styles.deltaDown : styles.deltaUp}>
              <span>{delta.direction === 'down' ? '▼' : '▲'}</span>
              {delta.pct !== null ? (
                `${delta.pct >= 0 ? '+' : ''}${delta.pct.toFixed(1)}%`
              ) : (
                <Money value={delta.absoluteMinorUnits} currency={account.currency} size="compact" />
              )}{' '}
              over {delta.periodLabel}
            </div>
          )}
        </div>
        <BalanceChart account={account} header={<NumberedBadge n={2} label="Trend" />} />
      </CornerBracketPanel>

      <div className={styles.grid3}>
        <div id="account-details" className={styles.card}>
          <NumberedBadge n={3} label="Account details" />
          <div className={styles.detailsTable}>
            <div className={styles.detailsKey}>IBAN</div>
            <div className={styles.detailsValRow}>
              {account ? (
                <>
                  <span className={styles.detailsVal}>{account.iban}</span>
                  <CopyButton value={account.iban} label="Copy IBAN" />
                </>
              ) : (
                <Skeleton className={styles.ibanSkeleton} />
              )}
            </div>
            <div className={styles.detailsKey}>Bank</div>
            <div className={styles.detailsVal}>Neo-Bank · fictional institution</div>
            <div className={styles.detailsKey}>Transfers</div>
            <div className={styles.detailsVal}>Neo-Bank accounts only</div>
          </div>
        </div>

        <div className={styles.card}>
          <NumberedBadge n={4} label="Money flow" />
          <MoneyFlowBar entries={entries} />
        </div>

        <div className={styles.card}>
          <NumberedBadge n={5} label="Actions" />
          <QuickActions layout="grid" />
        </div>
      </div>

      <BlueprintOperationTable entries={entries} isLoading={isLoading} isError={isError} onRetry={refetch} />

      <p className={styles.legend}>
        ① Balance&nbsp;&nbsp;② Trend&nbsp;&nbsp;③ Account details&nbsp;&nbsp;④ Money flow&nbsp;&nbsp;⑤ Quick actions&nbsp;&nbsp;⑥
        Operations
      </p>
    </div>
  )
}

// Split out so useFlashOnChange only mounts once account is actually
// loaded — otherwise the undefined-to-first-value transition on initial
// load would itself register as a "changed" balance and flash on mount.
function BlueprintBalanceValue({ account }: { account: MeResponse }) {
  const balanceFlash = useFlashOnChange(account.balance)
  return (
    <Money
      value={account.balance}
      currency={account.currency}
      showSign={false}
      size="hero"
      label="Balance"
      className={balanceFlash ? styles.balanceFlash : undefined}
    />
  )
}
