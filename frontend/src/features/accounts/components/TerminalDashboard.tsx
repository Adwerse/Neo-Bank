import { getAccessTokenEmail } from '../../../shared/api-client/jwt'
import { BellIcon } from '../../../shared/ui/icons'
import { CopyButton } from '../../../shared/ui/CopyButton'
import { Money } from '../../../shared/ui/Money'
import { Sparkline } from '../../../shared/ui/Sparkline'
import { Tag } from '../../../shared/ui/Tag'
import { useFlashOnChange } from '../../../shared/ui/useFlashOnChange'
import { LiveIndicator } from '../../../shared/ws-client/LiveIndicator'
import type { MeResponse } from '../api'
import { getDisplayName } from '../displayName'
import { computeBalanceDelta } from '../runningBalance'
import { ACCOUNT_STATUS_LABELS } from '../statusLabels'
import type { useDashboardOperations } from '../useDashboardOperations'
import { MoneyFlowBar } from './MoneyFlowBar'
import { QuickActions } from './QuickActions'
import { TerminalOperationList } from './TerminalOperationList'
import styles from './TerminalDashboard.module.css'

interface TerminalDashboardProps {
  account: MeResponse
  operations: ReturnType<typeof useDashboardOperations>
}

export function TerminalDashboard({ account, operations }: TerminalDashboardProps) {
  const balanceFlash = useFlashOnChange(account.balance)
  const { name, initial } = getDisplayName(getAccessTokenEmail())
  const { entries, balanceBeforeEarliest, isLoading, isError, refetch } = operations

  const earliestCreatedAt = entries[entries.length - 1]?.created_at
  const delta = computeBalanceDelta(account.balance, balanceBeforeEarliest, earliestCreatedAt)
  const fullSparkline = entries.length > 0 ? [balanceBeforeEarliest, ...[...entries].reverse().map((e) => e.balanceAfter)] : []

  return (
    <div className={styles.screen}>
      <div className={styles.wordmark}>Neo·Bank</div>

      <div className={styles.topRow}>
        <div className={styles.identity}>
          <div className={styles.avatar}>{initial}</div>
          <div>
            <div className={styles.greetingSmall}>С возвращением,</div>
            <div className={styles.greetingName}>{name}</div>
          </div>
        </div>
        <button type="button" className={styles.bellButton} aria-label="Уведомления">
          <BellIcon size={17} />
        </button>
      </div>

      <LiveIndicator />

      <div className={styles.balanceRow}>
        <div>
          <div className={styles.balanceLabel}>Баланс</div>
          <div className={styles.balanceValueRow}>
            <Money
              value={account.balance}
              currency={account.currency}
              showSign={false}
              size="hero"
              className={balanceFlash ? styles.balanceFlash : undefined}
            />
          </div>
          {delta.periodLabel && (
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
        {fullSparkline.length >= 2 && (
          <div className={styles.sparklineWrap}>
            <Sparkline values={fullSparkline} />
          </div>
        )}
      </div>

      <div id="account-details" className={styles.identityChips}>
        <Tag variant="accent">{ACCOUNT_STATUS_LABELS[account.status] ?? account.status}</Tag>
        <span className={styles.iban}>{account.iban}</span>
        <CopyButton value={account.iban} label="Скопировать IBAN" />
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
