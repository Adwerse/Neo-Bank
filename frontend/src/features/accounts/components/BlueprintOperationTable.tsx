import { Badge } from '../../../shared/ui/Badge'
import { Banner } from '../../../shared/ui/Banner'
import { Button } from '../../../shared/ui/Button'
import { Money } from '../../../shared/ui/Money'
import { NumberedBadge } from '../../../shared/ui/NumberedBadge'
import { Skeleton } from '../../../shared/ui/Skeleton'
import { Tag } from '../../../shared/ui/Tag'
import { useChangedRowKeys } from '../../../shared/ui/useChangedRowKeys'
import { isOutgoing, isPosted, rowKey, STATUS_LABELS, statusBadgeVariant, TYPE_LABELS } from '../../transfers/operationLabels'
import type { AnnotatedOperation } from '../runningBalance'
import styles from './BlueprintOperationTable.module.css'

interface BlueprintOperationTableProps {
  entries: AnnotatedOperation[]
  isLoading: boolean
  isError: boolean
  onRetry: () => void
}

export function BlueprintOperationTable({ entries, isLoading, isError, onRetry }: BlueprintOperationTableProps) {
  const changedKeys = useChangedRowKeys(entries, rowKey, (entry) => entry.updated_at)

  return (
    <div className={styles.card}>
      <div className={styles.header}>
        <NumberedBadge n={6} label="Операции" />
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
          <Button onClick={onRetry}>Повторить</Button>
        </div>
      )}

      {!isLoading && !isError && entries.length === 0 && <p className={styles.empty}>Операций пока нет.</p>}

      {!isLoading && !isError && entries.length > 0 && (
        <table className={styles.table}>
          <thead>
            <tr>
              <th className={styles.colNo}>№</th>
              <th>Операция</th>
              <th>Дата</th>
              <th>Статус</th>
              <th className={styles.right}>Сумма</th>
              <th className={styles.right}>Баланс</th>
            </tr>
          </thead>
          <tbody>
            {entries.map((entry, index) => {
              const outgoing = isOutgoing(entry)
              const posted = isPosted(entry)
              const key = rowKey(entry)
              return (
                <tr key={key} className={changedKeys.has(key) ? styles.rowChanged : undefined}>
                  <td className={styles.colNo}>{index + 1}</td>
                  <td>
                    {TYPE_LABELS[entry.type] ?? entry.type}
                    {entry.type === 'transfer' && entry.counterparty_iban && (
                      <span className={styles.counterparty}> · {entry.counterparty_iban}</span>
                    )}
                  </td>
                  <td className={styles.mono}>{new Date(entry.created_at).toLocaleString('ru-RU')}</td>
                  <td>
                    <Badge variant={statusBadgeVariant(entry)}>{STATUS_LABELS[entry.status] ?? entry.status}</Badge>
                  </td>
                  <td className={styles.right}>
                    <Money
                      value={outgoing ? -entry.amount : entry.amount}
                      currency="EUR"
                      tone={posted ? 'auto' : 'pending'}
                      size="compact"
                      className={styles.mono}
                    />
                  </td>
                  <td className={styles.right}>
                    <Money
                      value={entry.balanceAfter}
                      currency="EUR"
                      showSign={false}
                      tone="faint"
                      size="compact"
                      label="Баланс после операции"
                      className={styles.mono}
                    />
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      )}
    </div>
  )
}
