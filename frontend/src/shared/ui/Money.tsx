import { formatMoney } from '../money'
import styles from './Money.module.css'

interface MoneyProps {
  // Signed integer minor units. Positive = incoming/credit, negative =
  // outgoing/debit, zero = neutral. Formatting happens only here, at
  // render — callers hold the integer, never a pre-formatted string.
  value: number
  currency: string
  // false for balances/running-totals/category-sums, which have no
  // direction to claim. Default true.
  showSign?: boolean
  // 'auto' (default when showSign) colors by the real sign of `value` —
  // never color alone: the sign glyph below is the other channel.
  // 'neutral' is always var(--color-text), no directional meaning — a
  // balance or total that should read at full strength.
  // 'faint' is var(--color-text-faint) — a neutral amount that should
  // read as de-emphasized relative to its surroundings (a running-balance
  // column next to the amount that actually moved, say).
  // 'pending' is always var(--money-pending) regardless of sign — for a
  // not-yet-posted entry the sign is still honest (what WOULD happen), the
  // color just can't claim it already did.
  tone?: 'auto' | 'neutral' | 'faint' | 'pending'
  size?: 'hero' | 'compact'
  className?: string
}

export function Money({ value, currency, showSign = true, tone, size = 'compact', className }: MoneyProps) {
  const resolvedTone = tone ?? (showSign ? 'auto' : 'neutral')
  // Unicode minus (U+2212), not formatMoney's ASCII '-' — matches what
  // every hand-rolled call site already used. Zero never gets a sign: it
  // has no direction to claim either way.
  const sign = showSign && value > 0 ? '+' : showSign && value < 0 ? '−' : ''
  const body = formatMoney(Math.abs(value), currency)

  let toneClass = styles.neutral
  if (resolvedTone === 'pending') {
    toneClass = styles.pending
  } else if (resolvedTone === 'faint') {
    toneClass = styles.faint
  } else if (resolvedTone === 'auto') {
    if (value > 0) toneClass = styles.in
    else if (value < 0) toneClass = styles.out
  }

  // aria-label replaces the announced content outright, so a screen reader
  // never depends on how it happens to read the '+'/'−' glyph.
  const label = showSign && value !== 0 ? `${value > 0 ? 'Приход' : 'Расход'} ${body}` : undefined

  const classes = [styles.money, styles[size], toneClass, className].filter(Boolean).join(' ')

  return (
    <span className={classes} aria-label={label}>
      {sign}
      {body}
    </span>
  )
}
