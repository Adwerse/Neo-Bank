import { APP_LOCALE } from '../../shared/locale'

const MINUTE_MS = 60_000
const HOUR_MS = 60 * MINUTE_MS
const DAY_MS = 24 * HOUR_MS
const MAX_RELATIVE_DAYS = 30

const relativeFormat = new Intl.RelativeTimeFormat(APP_LOCALE, { numeric: 'auto' })
const shortDateFormat = new Intl.DateTimeFormat(APP_LOCALE, { day: '2-digit', month: '2-digit', year: 'numeric' })

function magnitude(ms: number): { value: number; unit: Intl.RelativeTimeFormatUnit } | null {
  if (ms < MINUTE_MS) return null // "just now" territory, no unit to pluralize
  if (ms < HOUR_MS) return { value: Math.floor(ms / MINUTE_MS), unit: 'minute' }
  if (ms < DAY_MS) return { value: Math.floor(ms / HOUR_MS), unit: 'hour' }
  return { value: Math.floor(ms / DAY_MS), unit: 'day' }
}

// Compact "N minutes ago" style, for the Terminal operation list's row
// subtext. Falls back to an absolute short date once it's further back
// than MAX_RELATIVE_DAYS — "47 days ago" reads worse than a date at that
// point. Intl.RelativeTimeFormat owns the wording (including English's
// two-form pluralization and any numeric:'auto' idioms like "yesterday")
// rather than a hand-rolled forms table.
export function formatRelativeTime(iso: string, now: Date = new Date()): string {
  const then = new Date(iso)
  const diffMs = now.getTime() - then.getTime()
  if (diffMs < MINUTE_MS) return 'just now'
  if (diffMs >= MAX_RELATIVE_DAYS * DAY_MS) {
    return shortDateFormat.format(then)
  }
  const m = magnitude(diffMs)
  if (!m) return 'just now'
  return relativeFormat.format(-m.value, m.unit)
}

// Same magnitude logic without the "ago" suffix, for the balance-delta
// line ("+27.3% over 12 days") — that line states a real span of whatever
// the fetched operations page actually covers, never a hardcoded period.
// Intl.RelativeTimeFormat has no "no suffix" mode, so this goes through
// Intl.NumberFormat's unit style instead — still Intl-owned pluralization
// ("1 day" vs "12 days"), just a different part of the API.
export function formatDurationSince(iso: string, now: Date = new Date()): string {
  const then = new Date(iso)
  const diffMs = Math.max(0, now.getTime() - then.getTime())
  const m = magnitude(diffMs)
  if (!m) return 'less than a minute'
  return new Intl.NumberFormat(APP_LOCALE, { style: 'unit', unit: m.unit, unitDisplay: 'long' }).format(m.value)
}
