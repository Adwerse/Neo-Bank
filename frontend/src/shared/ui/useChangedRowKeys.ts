import { useEffect, useRef, useState } from 'react'

const HIGHLIGHT_MS = 1500

// Diffs each refetch's entries against the previous render's by versionFn
// (e.g. updated_at), so a row that just transitioned (pending -> completed
// via the reconciliation worker, say) — or one that's brand new — flashes,
// while an untouched row sitting further down the list doesn't. React
// Query's default structural sharing means `entries` only gets a new array
// reference when something in it actually changed, so a fallback poll tick
// that returns identical data never trips this.
export function useChangedRowKeys<T>(
  entries: T[] | undefined,
  keyFn: (item: T) => string,
  versionFn: (item: T) => string,
): Set<string> {
  const prevRef = useRef<Map<string, string> | null>(null)
  const [changed, setChanged] = useState<Set<string>>(new Set())
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    if (!entries) return
    const prev = prevRef.current
    const next = new Map(entries.map((entry) => [keyFn(entry), versionFn(entry)]))
    if (prev) {
      const changedKeys = new Set<string>()
      for (const [key, version] of next) {
        if (prev.get(key) !== version) changedKeys.add(key)
      }
      if (changedKeys.size > 0) {
        setChanged(changedKeys)
        if (timerRef.current) clearTimeout(timerRef.current)
        timerRef.current = setTimeout(() => setChanged(new Set()), HIGHLIGHT_MS)
      }
    }
    prevRef.current = next
    // keyFn/versionFn are expected to be stable-enough inline closures (as
    // every call site here passes) — including them would re-run this on
    // every render and defeat the "only the previous render's snapshot"
    // comparison above.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [entries])

  useEffect(
    () => () => {
      if (timerRef.current) clearTimeout(timerRef.current)
    },
    [],
  )

  return changed
}
