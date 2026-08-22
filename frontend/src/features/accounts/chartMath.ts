export interface SparklinePoint {
  x: number
  y: number
}

export interface SparklinePaths {
  linePath: string
  // Same line, closed down to the bottom edge — for the gradient fill.
  areaPath: string
  points: SparklinePoint[]
}

// Pure SVG path generation shared by Sparkline.tsx and BlueprintChart.tsx so
// the two hand-rolled charts can't diverge in their math. No charting
// library — none is installed, and the source design hand-draws SVG paths
// too.
export function buildSparklinePath(values: number[], width: number, height: number): SparklinePaths {
  if (values.length === 0) {
    return { linePath: '', areaPath: '', points: [] }
  }
  if (values.length === 1) {
    const y = height / 2
    const points: SparklinePoint[] = [
      { x: 0, y },
      { x: width, y },
    ]
    const linePath = `M0,${y} L${width},${y}`
    return { linePath, areaPath: `${linePath} L${width},${height} L0,${height} Z`, points }
  }

  const min = Math.min(...values)
  const max = Math.max(...values)
  const range = max - min || 1 // a flat series (all-equal values) still draws a level line, not a /0
  const stepX = width / (values.length - 1)

  const points: SparklinePoint[] = values.map((value, i) => ({
    x: i * stepX,
    y: height - ((value - min) / range) * height,
  }))

  const linePath = points.map((p, i) => `${i === 0 ? 'M' : 'L'}${p.x.toFixed(1)},${p.y.toFixed(1)}`).join(' ')
  const areaPath = `${linePath} L${width.toFixed(1)},${height.toFixed(1)} L0,${height.toFixed(1)} Z`

  return { linePath, areaPath, points }
}

export interface Peak {
  index: number
  value: number
}

export function findPeak(values: number[]): Peak | null {
  if (values.length === 0) return null
  let index = 0
  for (let i = 1; i < values.length; i++) {
    if (values[i] > values[index]) index = i
  }
  return { index, value: values[index] }
}
