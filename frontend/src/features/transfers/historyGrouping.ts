import type { OperationHistoryEntry } from './api'
import { isPosted } from './operationLabels'

const DAY_MS = 86_400_000

function startOfDay(d: Date): Date {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate())
}

function capitalize(s: string): string {
  return s.length === 0 ? s : s[0].toUpperCase() + s.slice(1)
}

// "Сегодня" / "Вчера" / weekday name (this week) / short date otherwise —
// the backend gives no date-bucket of its own, so this only groups
// whatever page(s) the infinite-scroll feed has already loaded.
export function dateGroupLabel(iso: string, now: Date = new Date()): string {
  const d = new Date(iso)
  const diffDays = Math.round((startOfDay(now).getTime() - startOfDay(d).getTime()) / DAY_MS)
  if (diffDays === 0) return 'Сегодня'
  if (diffDays === 1) return 'Вчера'
  if (diffDays > 1 && diffDays < 7) return capitalize(d.toLocaleDateString('ru-RU', { weekday: 'long' }))
  return d.toLocaleDateString('ru-RU', {
    day: 'numeric',
    month: 'long',
    year: d.getFullYear() !== now.getFullYear() ? 'numeric' : undefined,
  })
}

export interface OperationGroup {
  label: string
  entries: OperationHistoryEntry[]
}

// Entries arrive newest-first already (GET /transfers's documented order),
// so a stable pass that opens a new group the first time a label is seen
// keeps that order — no separate sort needed.
export function groupByDate(entries: OperationHistoryEntry[], now: Date = new Date()): OperationGroup[] {
  const groups: OperationGroup[] = []
  const indexByLabel = new Map<string, number>()
  for (const entry of entries) {
    const label = dateGroupLabel(entry.created_at, now)
    let idx = indexByLabel.get(label)
    if (idx === undefined) {
      idx = groups.length
      indexByLabel.set(label, idx)
      groups.push({ label, entries: [] })
    }
    groups[idx].entries.push(entry)
  }
  return groups
}

export type TypeFilter = 'all' | 'transfer' | 'deposit' | 'withdrawal'
export type StatusFilter = 'all' | 'posted' | 'pending' | 'failed'
export type RangeFilter = 'all' | '7d' | '30d'

export interface OperationFilters {
  type: TypeFilter
  status: StatusFilter
  range: RangeFilter
}

export const DEFAULT_FILTERS: OperationFilters = { type: 'all', status: 'all', range: 'all' }

const FAILED_STATUSES = new Set(['failed', 'rejected'])

function matchesStatus(entry: OperationHistoryEntry, status: StatusFilter): boolean {
  switch (status) {
    case 'all':
      return true
    case 'failed':
      return FAILED_STATUSES.has(entry.status)
    case 'posted':
      return isPosted(entry)
    case 'pending':
      return !isPosted(entry) && !FAILED_STATUSES.has(entry.status)
  }
}

const RANGE_MS: Record<Exclude<RangeFilter, 'all'>, number> = {
  '7d': 7 * DAY_MS,
  '30d': 30 * DAY_MS,
}

// Applied client-side over whatever pages are currently loaded — GET
// /transfers only supports limit/cursor (no server-side type/status/date
// filtering), so a filter that excludes most of what's loaded just means
// "load more" surfaces fewer matches per page rather than none.
export function applyFilters(
  entries: OperationHistoryEntry[],
  filters: OperationFilters,
  now: Date = new Date(),
): OperationHistoryEntry[] {
  return entries.filter((entry) => {
    if (filters.type !== 'all' && entry.type !== filters.type) return false
    if (!matchesStatus(entry, filters.status)) return false
    if (filters.range !== 'all') {
      const age = now.getTime() - new Date(entry.created_at).getTime()
      if (age > RANGE_MS[filters.range]) return false
    }
    return true
  })
}
