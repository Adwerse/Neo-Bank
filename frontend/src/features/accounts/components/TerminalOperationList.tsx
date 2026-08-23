import { Badge } from '../../../shared/ui/Badge'
import { Banner } from '../../../shared/ui/Banner'
import { Button } from '../../../shared/ui/Button'
import { Money } from '../../../shared/ui/Money'
import { Skeleton } from '../../../shared/ui/Skeleton'
import { Tag } from '../../../shared/ui/Tag'
import { useChangedRowKeys } from '../../../shared/ui/useChangedRowKeys'
import { isOutgoing, isPosted, rowKey, STATUS_LABELS, statusBadgeVariant, TYPE_LABELS } from '../../transfers/operationLabels'
import { formatRelativeTime } from '../../transfers/relativeTime'
import type { AnnotatedOperation } from '../runningBalance'
import styles from './TerminalOperationList.module.css'

interface TerminalOperationListProps {
  entries: AnnotatedOperation[]
  isLoading: boolean
  isError: boolean
  onRetry: () => void
}

export function TerminalOperationList({ entries, isLoading, isError, onRetry }: TerminalOperationListProps) {
  const changedKeys = useChangedRowKeys(entries, rowKey, (entry) => entry.updated_at)

  return (
    <div>
      <div className={styles.header}>
        <span className={styles.title}>Последние операции</span>
        {!isLoading && !isError && <Tag variant="neutral">{entries.length}</Tag>}
      </div>

      {isLoading && (
        <div className={styles.skeletons}>
          <Skeleton className={styles.skeletonRow} />
          <Skeleton className={styles.skeletonRow} />
          <Skeleton className={styles.skeletonRow} />
        </div>
      )}

      {isError && (
        <div className={styles.errorBlock}>
          <Banner variant="warning">Не удалось загрузить операции.</Banner>
          <Button className={styles.retryButton} onClick={onRetry}>
            Повторить
          </Button>
        </div>
      )}

      {!isLoading && !isError && entries.length === 0 && <p className={styles.empty}>Операций пока нет.</p>}

      {!isLoading && !isError && entries.length > 0 && (
        <>
          <div className={styles.columnHeader}>
            <span className={styles.colOp}>Операция</span>
            <span className={styles.colAmt}>Сумма</span>
            <span className={styles.colBal}>Баланс</span>
          </div>
          <ul className={styles.rows}>
            {entries.map((entry) => {
              const outgoing = isOutgoing(entry)
              const posted = isPosted(entry)
              const key = rowKey(entry)
              const metaParts = [
                entry.type === 'transfer' ? entry.counterparty_iban : undefined,
                formatRelativeTime(entry.created_at),
              ].filter(Boolean)

              return (
                <li
                  key={key}
                  className={[styles.row, changedKeys.has(key) && styles.rowChanged].filter(Boolean).join(' ')}
                >
                  <div className={styles.colOp}>
                    <div className={styles.opName}>
                      {TYPE_LABELS[entry.type] ?? entry.type}
                      {entry.type === 'withdrawal' && <span className={styles.simulationTag}> · симуляция</span>}
                    </div>
                    <div className={styles.opMeta}>
                      <span className={styles.opMetaText}>{metaParts.join(' · ')}</span>
                      {!posted && (
                        <Badge variant={statusBadgeVariant(entry)} className={styles.statusBadge}>
                          {STATUS_LABELS[entry.status] ?? entry.status}
                        </Badge>
                      )}
                    </div>
                  </div>
                  <Money
                    value={outgoing ? -entry.amount : entry.amount}
                    currency="EUR"
                    tone={posted ? 'auto' : 'pending'}
                    size="compact"
                    className={styles.colAmt}
                  />
                  <Money
                    value={entry.balanceAfter}
                    currency="EUR"
                    showSign={false}
                    tone="faint"
                    size="compact"
                    className={styles.colBal}
                  />
                </li>
              )
            })}
          </ul>
        </>
      )}
    </div>
  )
}
