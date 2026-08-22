import { CopyButton } from '../../../shared/ui/CopyButton'
import { CornerBracketPanel } from '../../../shared/ui/CornerBracketPanel'
import { NumberedBadge } from '../../../shared/ui/NumberedBadge'
import { useFlashOnChange } from '../../../shared/ui/useFlashOnChange'
import type { MeResponse } from '../api'
import { formatMoney } from '../money'
import { computeBalanceDelta } from '../runningBalance'
import { ACCOUNT_STATUS_LABELS } from '../statusLabels'
import type { useDashboardOperations } from '../useDashboardOperations'
import { BlueprintChart } from './BlueprintChart'
import { BlueprintOperationTable } from './BlueprintOperationTable'
import { MoneyFlowBar } from './MoneyFlowBar'
import { QuickActions } from './QuickActions'
import styles from './BlueprintDashboard.module.css'

interface BlueprintDashboardProps {
  account: MeResponse
  operations: ReturnType<typeof useDashboardOperations>
}

const STATUS_COLOR_CLASS: Record<string, string> = {
  active: 'statusActive',
  frozen: 'statusFrozen',
  closed: 'statusClosed',
}

export function BlueprintDashboard({ account, operations }: BlueprintDashboardProps) {
  const balanceFlash = useFlashOnChange(account.balance)
  const { entries, balanceBeforeEarliest, isLoading, isError, refetch } = operations
  const earliestCreatedAt = entries[entries.length - 1]?.created_at
  const delta = computeBalanceDelta(account.balance, balanceBeforeEarliest, earliestCreatedAt)
  const statusClass = styles[STATUS_COLOR_CLASS[account.status] ?? 'statusClosed']

  return (
    <div className={styles.page}>
      <div className={styles.infoStrip}>
        <div className={styles.infoCell}>
          <div className={styles.infoLabel}>Счёт</div>
          <div className={styles.infoValue}>{account.account_number}</div>
        </div>
        <div className={styles.infoCell}>
          <div className={styles.infoLabel}>Валюта</div>
          <div className={styles.infoValue}>{account.currency}</div>
        </div>
        <div className={styles.infoCell}>
          <div className={styles.infoLabel}>Статус</div>
          <div className={[styles.infoValue, statusClass].join(' ')}>{ACCOUNT_STATUS_LABELS[account.status] ?? account.status}</div>
        </div>
        <div className={styles.infoCell}>
          <div className={styles.infoLabel}>Тип</div>
          {/* Every account in this system is the same single product — a
              true constant, not a fabricated per-account field. */}
          <div className={styles.infoValue}>Текущий</div>
        </div>
        <div className={styles.infoCellLast}>
          <div className={styles.infoLabel}>Открыт</div>
          <div className={styles.infoValue}>{new Date(account.created_at).toLocaleDateString('ru-RU')}</div>
        </div>
      </div>

      <CornerBracketPanel>
        <div className={styles.balancePanel}>
          <NumberedBadge n={1} label="Баланс" />
          <div className={styles.balanceValueRow}>
            <span className={[styles.balanceValue, balanceFlash && styles.balanceFlash].filter(Boolean).join(' ')}>
              {formatMoney(account.balance, account.currency)}
            </span>
          </div>
          <svg className={styles.dimensionLine} viewBox="0 0 220 14" preserveAspectRatio="none" aria-hidden="true">
            <line x1="1" y1="7" x2="219" y2="7" stroke="var(--color-accent)" strokeWidth="1" />
            <line x1="1" y1="2" x2="1" y2="12" stroke="var(--color-accent)" strokeWidth="1" />
            <line x1="219" y1="2" x2="219" y2="12" stroke="var(--color-accent)" strokeWidth="1" />
          </svg>
          {delta.periodLabel && (
            <div className={delta.direction === 'down' ? styles.deltaDown : styles.deltaUp}>
              <span>{delta.direction === 'down' ? '▼' : '▲'}</span>
              {delta.pct !== null
                ? `${delta.pct >= 0 ? '+' : ''}${delta.pct.toFixed(1)}%`
                : formatMoney(delta.absoluteMinorUnits, account.currency)}{' '}
              за {delta.periodLabel}
            </div>
          )}
        </div>
        <BlueprintChart entries={entries} balanceBeforeEarliest={balanceBeforeEarliest} />
      </CornerBracketPanel>

      <div className={styles.grid3}>
        <div id="account-details" className={styles.card}>
          <NumberedBadge n={3} label="Реквизиты счёта" />
          <div className={styles.detailsTable}>
            <div className={styles.detailsKey}>IBAN</div>
            <div className={styles.detailsValRow}>
              <span className={styles.detailsVal}>{account.iban}</span>
              <CopyButton value={account.iban} label="Скопировать IBAN" />
            </div>
            <div className={styles.detailsKey}>Банк</div>
            <div className={styles.detailsVal}>Neo-Bank · фиктивный институт</div>
            <div className={styles.detailsKey}>Переводы</div>
            <div className={styles.detailsVal}>только между счетами Neo-Bank</div>
          </div>
        </div>

        <div className={styles.card}>
          <NumberedBadge n={4} label="Движение средств" />
          <MoneyFlowBar entries={entries} />
        </div>

        <div className={styles.card}>
          <NumberedBadge n={5} label="Действия" />
          <QuickActions layout="grid" />
        </div>
      </div>

      <BlueprintOperationTable entries={entries} isLoading={isLoading} isError={isError} onRetry={refetch} />

      <p className={styles.legend}>
        ① Баланс&nbsp;&nbsp;② Динамика&nbsp;&nbsp;③ Реквизиты счёта&nbsp;&nbsp;④ Движение средств&nbsp;&nbsp;⑤ Быстрые действия&nbsp;&nbsp;⑥
        Операции
      </p>
    </div>
  )
}
