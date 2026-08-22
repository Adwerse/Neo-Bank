import { useId, useMemo, useState } from 'react'
import { NumberedBadge } from '../../../shared/ui/NumberedBadge'
import { buildSparklinePath, findPeak } from '../chartMath'
import type { AnnotatedOperation } from '../runningBalance'
import styles from './BlueprintChart.module.css'

const WIDTH = 600
const HEIGHT = 200
const PAD_TOP = 14
const PAD_BOTTOM = 24
const INNER_HEIGHT = HEIGHT - PAD_TOP - PAD_BOTTOM

const DAY_MS = 24 * 60 * 60 * 1000

const RANGES: { key: string; days: number }[] = [
  { key: '1Н', days: 7 },
  { key: '1М', days: 30 },
  { key: '3М', days: 90 },
  { key: '6М', days: 180 },
  { key: '1Г', days: 365 },
]

function formatAxisLabel(minorUnits: number): string {
  const euros = minorUnits / 100
  if (Math.abs(euros) >= 1000) {
    return `${(euros / 1000).toFixed(1).replace(/\.0$/, '')}K`
  }
  return Math.round(euros).toString()
}

function formatShortDate(iso: string): string {
  return new Date(iso).toLocaleDateString('ru-RU', { day: '2-digit', month: '2-digit' })
}

interface BlueprintChartProps {
  entries: AnnotatedOperation[] // newest-first, full fetched page
  balanceBeforeEarliest: number
}

export function BlueprintChart({ entries, balanceBeforeEarliest }: BlueprintChartProps) {
  const [selectedRange, setSelectedRange] = useState('6М')
  const rawId = useId()
  const gradientId = `blueprint-fill-${rawId.replace(/[^a-zA-Z0-9]/g, '')}`

  const { values, startLabel, endLabel, hitFetchLimit } = useMemo(() => {
    const chronological = [...entries].reverse() // API order is newest-first; a chart must read left-to-right in time
    const range = RANGES.find((r) => r.key === selectedRange) ?? RANGES[3]
    const cutoffMs = Date.now() - range.days * DAY_MS
    const startIdx = chronological.findIndex((entry) => new Date(entry.created_at).getTime() >= cutoffMs)

    if (startIdx === -1) {
      return { values: [balanceBeforeEarliest], windowEntries: [], startLabel: '', endLabel: '', hitFetchLimit: false }
    }

    const windowEntries = chronological.slice(startIdx)
    // The true balance at the start of this window — not just "after the
    // oldest included entry" — so a range filter never silently loses the
    // starting point's real value, even when it cuts older history away.
    const leadingBalance = startIdx === 0 ? balanceBeforeEarliest : chronological[startIdx - 1].balanceAfter
    const vals = [leadingBalance, ...windowEntries.map((entry) => entry.balanceAfter)]
    return {
      values: vals,
      windowEntries,
      startLabel: formatShortDate(windowEntries[0].created_at),
      endLabel: formatShortDate(windowEntries[windowEntries.length - 1].created_at),
      // startIdx===0 means this window already includes every operation we
      // fetched — a longer range can't show more without a bigger fetch, so
      // it would render identically. Only a real limitation of a bounded
      // fetch, never hidden from the range control's own behavior.
      hitFetchLimit: startIdx === 0,
    }
  }, [entries, selectedRange, balanceBeforeEarliest])

  const { linePath, areaPath, points } = buildSparklinePath(values, WIDTH, INNER_HEIGHT)
  const peak = findPeak(values)
  const min = values.length > 0 ? Math.min(...values) : 0
  const max = values.length > 0 ? Math.max(...values) : 0
  const mid = (min + max) / 2
  const last = points[points.length - 1]
  const peakPoint = peak ? points[peak.index] : null
  // Skip a separate peak callout when the peak IS the current point — the
  // adjacent balance panel already states that value, so a second label on
  // top of the end-dot would just duplicate it.
  const showPeakCallout = peak && peakPoint && peak.index !== values.length - 1

  return (
    <div className={styles.wrap}>
      <div className={styles.header}>
        <NumberedBadge n={2} label={`Динамика · ${selectedRange}`} />
        <div className={styles.seg}>
          {RANGES.map((range) => (
            <button
              key={range.key}
              type="button"
              className={range.key === selectedRange ? styles.segOptActive : styles.segOpt}
              onClick={() => setSelectedRange(range.key)}
            >
              {range.key}
            </button>
          ))}
        </div>
      </div>

      {values.length < 2 ? (
        <p className={styles.empty}>Недостаточно операций за этот период.</p>
      ) : (
        <>
          <svg
            width="100%"
            height={HEIGHT}
            viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
            preserveAspectRatio="none"
            className={styles.svg}
          >
            <defs>
              <pattern id={`${gradientId}-grid`} width="30" height="30" patternUnits="userSpaceOnUse">
                <path d="M30 0H0V30" fill="none" stroke="var(--color-divider)" strokeWidth="1" />
              </pattern>
              <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
                <stop offset="0%" stopColor="var(--color-accent)" stopOpacity="0.22" />
                <stop offset="100%" stopColor="var(--color-accent)" stopOpacity="0" />
              </linearGradient>
            </defs>

            <rect x="0" y="0" width={WIDTH} height={HEIGHT} fill={`url(#${gradientId}-grid)`} />

            <g transform={`translate(0, ${PAD_TOP})`}>
              <line x1="0" y1="0" x2={WIDTH} y2="0" stroke="var(--color-divider)" strokeWidth="1.2" />
              <line x1="0" y1={INNER_HEIGHT / 2} x2={WIDTH} y2={INNER_HEIGHT / 2} stroke="var(--color-divider)" strokeWidth="1.2" />
              <line x1="0" y1={INNER_HEIGHT} x2={WIDTH} y2={INNER_HEIGHT} stroke="var(--color-divider)" strokeWidth="1.2" />
              <text x="4" y="-3" className={styles.axisLabel}>
                {formatAxisLabel(max)}
              </text>
              <text x="4" y={INNER_HEIGHT / 2 - 3} className={styles.axisLabel}>
                {formatAxisLabel(mid)}
              </text>
              <text x="4" y={INNER_HEIGHT - 3} className={styles.axisLabel}>
                {formatAxisLabel(min)}
              </text>

              <path d={areaPath} fill={`url(#${gradientId})`} stroke="none" />
              <path d={linePath} fill="none" stroke="var(--color-accent)" strokeWidth="2" strokeLinejoin="round" strokeLinecap="round" />

              {showPeakCallout && peakPoint && (
                <>
                  <line
                    x1={peakPoint.x}
                    y1={peakPoint.y}
                    x2={peakPoint.x}
                    y2={INNER_HEIGHT}
                    stroke="var(--color-accent-400)"
                    strokeWidth="1"
                    strokeDasharray="2 3"
                  />
                  <circle cx={peakPoint.x} cy={peakPoint.y} r="3" fill="var(--color-surface)" stroke="var(--color-accent)" strokeWidth="1.5" />
                  <text
                    x={peakPoint.x}
                    y={Math.max(9, peakPoint.y - 8)}
                    textAnchor={peakPoint.x > WIDTH / 2 ? 'end' : 'start'}
                    className={styles.peakLabel}
                  >
                    МАКС {formatAxisLabel(peak.value)}
                  </text>
                </>
              )}

              {last && <circle cx={last.x} cy={last.y} r="3.2" fill="var(--color-accent)" />}
            </g>
          </svg>
          <div className={styles.axisDates}>
            <span>{startLabel}</span>
            <span>{endLabel || 'Сегодня'}</span>
          </div>
          {hitFetchLimit && selectedRange !== '1Н' && (
            <p className={styles.rangeNote}>Показана вся доступная история — более длинный период выглядел бы так же.</p>
          )}
        </>
      )}
    </div>
  )
}
