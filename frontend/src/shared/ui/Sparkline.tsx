import { useId } from 'react'
import { buildSparklinePath } from '../../features/accounts/chartMath'
import styles from './Sparkline.module.css'

interface SparklineProps {
  values: number[]
  width?: number
  height?: number
}

export function Sparkline({ values, width = 110, height = 40 }: SparklineProps) {
  const rawId = useId()
  const gradientId = `spark-${rawId.replace(/[^a-zA-Z0-9]/g, '')}`

  if (values.length < 2) return null

  const { linePath, areaPath, points } = buildSparklinePath(values, width, height)
  // Color reflects the real trend direction (first fetched point to last) —
  // never hardcoded to green.
  const trendUp = values[values.length - 1] >= values[0]
  const color = trendUp ? 'var(--money-in)' : 'var(--money-out)'
  const last = points[points.length - 1]

  return (
    <svg width={width} height={height} viewBox={`0 0 ${width} ${height}`} className={styles.svg} aria-hidden="true">
      <defs>
        <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor={color} stopOpacity="0.3" />
          <stop offset="100%" stopColor={color} stopOpacity="0" />
        </linearGradient>
      </defs>
      <path d={areaPath} fill={`url(#${gradientId})`} stroke="none" />
      <path d={linePath} fill="none" stroke={color} strokeWidth="1.6" strokeLinejoin="round" strokeLinecap="round" />
      {last && <circle cx={last.x} cy={last.y} r="2.4" fill={color} />}
    </svg>
  )
}
