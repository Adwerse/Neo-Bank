import { useEffect, useMemo, useRef, useState } from 'react'
import { isApiError } from '../../../shared/api-client/ApiError'
import { Badge } from '../../../shared/ui/Badge'
import { Card } from '../../../shared/ui/Card'
import { Banner } from '../../../shared/ui/Banner'
import { Button } from '../../../shared/ui/Button'
import { Money } from '../../../shared/ui/Money'
import { Skeleton } from '../../../shared/ui/Skeleton'
import { useChangedRowKeys } from '../../../shared/ui/useChangedRowKeys'
import { useOperationHistory } from '../useOperationHistory'
import { TYPE_LABELS, STATUS_LABELS, isOutgoing, isPosted, rowKey, statusBadgeVariant } from '../operationLabels'
import {
  applyFilters,
  groupByDate,
  DEFAULT_FILTERS,
  type OperationFilters,
  type RangeFilter,
  type StatusFilter,
  type TypeFilter,
} from '../historyGrouping'
import styles from './OperationHistory.module.css'

const TYPE_OPTIONS: { value: TypeFilter; label: string }[] = [
  { value: 'all', label: 'Все' },
  { value: 'transfer', label: 'Переводы' },
  { value: 'deposit', label: 'Депозиты' },
  { value: 'withdrawal', label: 'Выводы' },
]

const STATUS_OPTIONS: { value: StatusFilter; label: string }[] = [
  { value: 'all', label: 'Любой статус' },
  { value: 'posted', label: 'Выполнено' },
  { value: 'pending', label: 'В обработке' },
  { value: 'failed', label: 'Не выполнено' },
]

const RANGE_OPTIONS: { value: RangeFilter; label: string }[] = [
  { value: 'all', label: 'Всё время' },
  { value: '7d', label: '7 дней' },
  { value: '30d', label: '30 дней' },
]

function FilterGroup<T extends string>({
  options,
  value,
  onChange,
}: {
  options: { value: T; label: string }[]
  value: T
  onChange: (v: T) => void
}) {
  return (
    <div className={styles.filterGroup} role="group">
      {options.map((opt) => (
        <button
          key={opt.value}
          type="button"
          className={[styles.filterPill, opt.value === value && styles.filterPillActive].filter(Boolean).join(' ')}
          aria-pressed={opt.value === value}
          onClick={() => onChange(opt.value)}
        >
          {opt.label}
        </button>
      ))}
    </div>
  )
}

export function OperationHistory() {
  const { data, isLoading, isError, error, refetch, fetchNextPage, hasNextPage, isFetchingNextPage } =
    useOperationHistory()
  const [filters, setFilters] = useState<OperationFilters>(DEFAULT_FILTERS)
  const [nextPageError, setNextPageError] = useState(false)
  const sentinelRef = useRef<HTMLDivElement | null>(null)

  const allEntries = useMemo(() => data?.pages.flatMap((page) => page.transfers) ?? [], [data])
  const filteredEntries = useMemo(() => applyFilters(allEntries, filters), [allEntries, filters])
  const groups = useMemo(() => groupByDate(filteredEntries), [filteredEntries])

  // Diffed against allEntries (not filteredEntries) so toggling a filter —
  // which changes the array reference without any row's data actually
  // changing — never triggers a spurious flash. Called unconditionally
  // (before the loading/error early returns below) per the rules of hooks.
  const changedKeys = useChangedRowKeys(allEntries, rowKey, (entry) => entry.updated_at)

  async function loadMore() {
    setNextPageError(false)
    try {
      await fetchNextPage()
    } catch {
      setNextPageError(true)
    }
  }

  // Auto-loads the next page once the sentinel scrolls into view — the
  // "infinite" half of infinite scroll. A manual "Показать ещё" button
  // below covers the case where IntersectionObserver isn't available or
  // the sentinel never enters the viewport (e.g. a very short list).
  useEffect(() => {
    const node = sentinelRef.current
    if (!node || !hasNextPage || typeof IntersectionObserver === 'undefined') return
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries[0]?.isIntersecting && !isFetchingNextPage) {
          loadMore()
        }
      },
      { rootMargin: '200px' },
    )
    observer.observe(node)
    return () => observer.disconnect()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [hasNextPage, isFetchingNextPage, filteredEntries.length])

  if (isLoading) {
    return (
      <Card>
        <h2>История операций</h2>
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
        : 'Не удалось загрузить историю операций.'
    return (
      <Card>
        <h2>История операций</h2>
        <Banner variant="warning">{message}</Banner>
        <Button className={styles.retryButton} onClick={() => refetch()}>
          Повторить
        </Button>
      </Card>
    )
  }

  const hasAnyFilterApplied = filters.type !== 'all' || filters.status !== 'all' || filters.range !== 'all'

  return (
    <Card>
      <h2>История операций</h2>

      {allEntries.length > 0 && (
        <div className={styles.filters}>
          <FilterGroup options={TYPE_OPTIONS} value={filters.type} onChange={(type) => setFilters((f) => ({ ...f, type }))} />
          <FilterGroup
            options={STATUS_OPTIONS}
            value={filters.status}
            onChange={(status) => setFilters((f) => ({ ...f, status }))}
          />
          <FilterGroup options={RANGE_OPTIONS} value={filters.range} onChange={(range) => setFilters((f) => ({ ...f, range }))} />
        </div>
      )}

      {allEntries.length === 0 ? (
        <p className={styles.empty}>Операций пока нет.</p>
      ) : filteredEntries.length === 0 ? (
        <div className={styles.empty}>
          <p>Ничего не найдено по выбранным фильтрам.</p>
          {hasAnyFilterApplied && (
            <Button variant="secondary" onClick={() => setFilters(DEFAULT_FILTERS)}>
              Сбросить фильтры
            </Button>
          )}
        </div>
      ) : (
        <>
          {groups.map((group) => (
            <section key={group.label} className={styles.group}>
              <h3 className={styles.groupLabel}>{group.label}</h3>
              <ul className={styles.list}>
                {group.entries.map((entry) => {
                  const outgoing = isOutgoing(entry)
                  const posted = isPosted(entry)
                  const key = rowKey(entry)
                  return (
                    <li
                      key={key}
                      className={[styles.row, changedKeys.has(key) && styles.rowChanged].filter(Boolean).join(' ')}
                    >
                      <span className={styles.typeBadge}>
                        {TYPE_LABELS[entry.type] ?? entry.type}
                        {entry.type === 'withdrawal' && <span className={styles.simulationTag}> · симуляция</span>}
                      </span>
                      <Money
                        value={outgoing ? -entry.amount : entry.amount}
                        currency="EUR"
                        tone={posted ? 'auto' : 'pending'}
                        size="compact"
                      />
                      <span className={styles.counterparty}>
                        {entry.type === 'transfer' ? entry.counterparty_iban : '—'}
                      </span>
                      <Badge variant={statusBadgeVariant(entry)}>{STATUS_LABELS[entry.status] ?? entry.status}</Badge>
                      <span className={styles.date}>
                        {new Date(entry.created_at).toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit' })}
                      </span>
                    </li>
                  )
                })}
              </ul>
            </section>
          ))}

          <div ref={sentinelRef} className={styles.sentinel} aria-hidden="true" />

          {isFetchingNextPage && <Skeleton className={styles.skeletonRow} />}

          {nextPageError && (
            <div className={styles.loadMoreError}>
              <Banner variant="warning">Не удалось загрузить ещё операции.</Banner>
              <Button variant="secondary" onClick={loadMore}>
                Повторить
              </Button>
            </div>
          )}

          {!isFetchingNextPage && !nextPageError && hasNextPage && (
            <Button variant="secondary" className={styles.loadMoreButton} onClick={loadMore}>
              Показать ещё
            </Button>
          )}
        </>
      )}
    </Card>
  )
}
