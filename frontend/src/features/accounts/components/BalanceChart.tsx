import { useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { Area, AreaChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'
import { Link } from 'react-router'
import buttonStyles from '../../../shared/ui/Button.module.css'
import { Skeleton } from '../../../shared/ui/Skeleton'
import { usePrefersReducedMotion } from '../../../shared/ui/usePrefersReducedMotion'
import { formatMoney } from '../../../shared/money'
import { APP_LOCALE } from '../../../shared/locale'
import type { BalanceHistoryRange, BalanceHistoryResponse, MeResponse } from '../api'
import { useBalanceHistory } from '../useBalanceHistory'
import styles from './BalanceChart.module.css'

type BalancePoint = BalanceHistoryResponse['points'][number]

const RANGES: { key: BalanceHistoryRange; label: string }[] = [
  { key: 'week', label: 'Week' },
  { key: 'month', label: 'Month' },
  { key: 'all', label: 'All' },
]

function todayDateString(): string {
  return new Date().toISOString().slice(0, 10)
}

// The series ledger-svc returns ends at the last day something happened —
// never "today" unless something happened today. A chart reading left to
// right should still visibly reach the present, so this appends a point
// carrying the balance forward, purely for display. Never cached, never
// sent back to any query — computed fresh on every render.
function extendToToday(raw: BalancePoint[]): BalancePoint[] {
  if (raw.length === 0) return raw
  const today = todayDateString()
  const last = raw[raw.length - 1]
  return last.date < today ? [...raw, { date: today, balance: last.balance }] : raw
}

function formatAxisValue(minorUnits: number): string {
  const units = minorUnits / 100
  if (Math.abs(units) >= 1000) {
    return `${(units / 1000).toFixed(1).replace(/\.0$/, '')}K`
  }
  return Math.round(units).toString()
}

const axisDateFormat = new Intl.DateTimeFormat(APP_LOCALE, { day: '2-digit', month: '2-digit' })

function formatAxisDate(dateStr: string): string {
  // dateStr is a plain 'YYYY-MM-DD' with no time component — parsing it via
  // `new Date(dateStr)` would read it as UTC midnight and can shift a day
  // backward in a negative-offset zone, so it's built as local midnight.
  const [year, month, day] = dateStr.split('-').map(Number)
  return axisDateFormat.format(new Date(year, month - 1, day))
}

interface BalanceChartProps {
  account: MeResponse | undefined
  // Theme-specific title chrome (a NumberedBadge for Blueprint, a plain
  // section label for Terminal) — BalanceChart owns the period toggle and
  // the chart itself, but not the surrounding page's visual language for a
  // block title, matching how MoneyFlowBar/QuickActions split that
  // responsibility with their callers.
  header: ReactNode
}

export function BalanceChart({ account, header }: BalanceChartProps) {
  const [range, setRange] = useState<BalanceHistoryRange>('week')
  const { data, isLoading, isError, refetch } = useBalanceHistory(range)
  const reducedMotion = usePrefersReducedMotion()

  const points = useMemo(() => extendToToday(data?.points ?? []), [data])
  // A window where every visible point is 0 reads as an empty chart to the
  // user no matter which range they picked or whether the account has ever
  // had activity outside that window — so this doesn't need account.balance
  // as a separate signal.
  const isEmpty = points.length === 0 || points.every((p) => p.balance === 0)

  return (
    <div className={styles.wrap}>
      <div className={styles.header}>
        {header}
        <div className={styles.seg}>
          {RANGES.map((r) => (
            <button
              key={r.key}
              type="button"
              className={r.key === range ? styles.segOptActive : styles.segOpt}
              onClick={() => setRange(r.key)}
            >
              {r.label}
            </button>
          ))}
        </div>
      </div>
      <BalanceChartBody
        account={account}
        isLoading={isLoading}
        isError={isError}
        onRetry={refetch}
        points={points}
        isEmpty={isEmpty}
        reducedMotion={reducedMotion}
      />
    </div>
  )
}

interface BalanceChartBodyProps {
  account: MeResponse | undefined
  isLoading: boolean
  isError: boolean
  onRetry: () => void
  points: BalancePoint[]
  isEmpty: boolean
  reducedMotion: boolean
}

function BalanceChartBody({ account, isLoading, isError, onRetry, points, isEmpty, reducedMotion }: BalanceChartBodyProps) {
  if (!account || isLoading) {
    return <Skeleton className={styles.skeletonChart} />
  }

  if (isError) {
    return (
      <div className={styles.errorState}>
        <p>Failed to load balance history.</p>
        <button type="button" className={styles.retryLink} onClick={onRetry}>
          Retry
        </button>
      </div>
    )
  }

  if (isEmpty) {
    return (
      <div className={styles.emptyState}>
        <p className={styles.emptyText}>Top up your account to see the balance trend.</p>
        <Link to="/deposit" className={`${buttonStyles.button} ${buttonStyles.primary}`}>
          Top up account
        </Link>
      </div>
    )
  }

  if (points.length < 2) {
    return <p className={styles.empty}>Not enough data for this period.</p>
  }

  return (
    <ResponsiveContainer width="100%" height={200}>
      <AreaChart data={points} margin={{ top: 8, right: 4, left: 0, bottom: 0 }}>
        <defs>
          <linearGradient id="balance-chart-fill" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="var(--color-accent)" stopOpacity={0.22} />
            <stop offset="100%" stopColor="var(--color-accent)" stopOpacity={0} />
          </linearGradient>
        </defs>
        <XAxis
          dataKey="date"
          tickFormatter={formatAxisDate}
          tick={{ fill: 'var(--color-text-faint)', fontSize: 10.5 }}
          tickLine={false}
          axisLine={{ stroke: 'var(--color-divider)' }}
          minTickGap={28}
        />
        <YAxis
          tickFormatter={formatAxisValue}
          tick={{ fill: 'var(--color-text-faint)', fontSize: 10.5 }}
          tickLine={false}
          axisLine={false}
          width={38}
        />
        <Tooltip
          content={({ active, payload, label }) => {
            if (!active || !payload || payload.length === 0) return null
            const value = payload[0]?.value
            if (typeof value !== 'number') return null
            return (
              <div className={styles.tooltip}>
                <div className={styles.tooltipDate}>{formatAxisDate(String(label))}</div>
                <div className={styles.tooltipValue}>{formatMoney(value, account.currency)}</div>
              </div>
            )
          }}
        />
        <Area
          type="monotone"
          dataKey="balance"
          stroke="var(--color-accent)"
          strokeWidth={2}
          fill="url(#balance-chart-fill)"
          isAnimationActive={!reducedMotion}
        />
      </AreaChart>
    </ResponsiveContainer>
  )
}
