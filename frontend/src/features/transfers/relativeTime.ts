const MINUTE_MS = 60_000
const HOUR_MS = 60 * MINUTE_MS
const DAY_MS = 24 * HOUR_MS
const MAX_RELATIVE_DAYS = 30

// Standard Russian pluralization: 1 -> one, 2-4 -> few, 0/5-20 -> many
// (11-14 are the "many" exception inside what would otherwise read as
// "few"). forms = [one, few, many], e.g. ['день', 'дня', 'дней'].
function ruPlural(n: number, forms: [string, string, string]): string {
  const mod100 = Math.abs(n) % 100
  const mod10 = mod100 % 10
  if (mod100 > 10 && mod100 < 20) return forms[2]
  if (mod10 === 1) return forms[0]
  if (mod10 > 1 && mod10 < 5) return forms[1]
  return forms[2]
}

function magnitude(ms: number): { value: number; forms: [string, string, string] } | null {
  if (ms < MINUTE_MS) return null // "just now" territory, no unit to pluralize
  if (ms < HOUR_MS) return { value: Math.floor(ms / MINUTE_MS), forms: ['минута', 'минуты', 'минут'] }
  if (ms < DAY_MS) return { value: Math.floor(ms / HOUR_MS), forms: ['час', 'часа', 'часов'] }
  return { value: Math.floor(ms / DAY_MS), forms: ['день', 'дня', 'дней'] }
}

// Compact "N дней назад" style, for the Terminal operation list's row
// subtext. Falls back to an absolute short date once it's further back than
// MAX_RELATIVE_DAYS — "47 дней назад" reads worse than a date at that point.
export function formatRelativeTime(iso: string, now: Date = new Date()): string {
  const then = new Date(iso)
  const diffMs = now.getTime() - then.getTime()
  if (diffMs < MINUTE_MS) return 'только что'
  if (diffMs >= MAX_RELATIVE_DAYS * DAY_MS) {
    return then.toLocaleDateString('ru-RU', { day: '2-digit', month: '2-digit', year: 'numeric' })
  }
  const m = magnitude(diffMs)
  if (!m) return 'только что'
  return `${m.value} ${ruPlural(m.value, m.forms)} назад`
}

// Same magnitude logic without the "назад" suffix, for the balance-delta
// line ("+27.3% за 12 дней") — that line states a real span of whatever the
// fetched operations page actually covers, never a hardcoded period.
export function formatDurationSince(iso: string, now: Date = new Date()): string {
  const then = new Date(iso)
  const diffMs = Math.max(0, now.getTime() - then.getTime())
  const m = magnitude(diffMs)
  if (!m) return 'меньше минуты'
  return `${m.value} ${ruPlural(m.value, m.forms)}`
}
