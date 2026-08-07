import type { QueryClient, QueryKey } from '@tanstack/react-query'

// A transfer completing pushes balance.changed AND transfer.updated as two
// separate WS frames (see gateway/notify.go's signalsForTransferEvent) —
// without batching, that's two invalidateQueries calls back to back, and a
// deposit crediting can add a third. Queuing by key and flushing once after
// a short quiet window collapses any such burst into one pass, regardless
// of how many distinct signals produced it.
const DEBOUNCE_MS = 150

export interface InvalidationBatcher {
  queue: (key: QueryKey) => void
  dispose: () => void
}

export function createInvalidationBatcher(queryClient: QueryClient): InvalidationBatcher {
  // Keyed by the JSON form so two queue() calls for the same logical key
  // (e.g. ['accounts','me'] from both a balance.changed and a
  // deposit.updated in the same burst) collapse to one entry instead of
  // invalidating twice.
  const pending = new Map<string, QueryKey>()
  let timer: ReturnType<typeof setTimeout> | null = null

  function flush() {
    timer = null
    for (const key of pending.values()) {
      queryClient.invalidateQueries({ queryKey: key })
    }
    pending.clear()
  }

  return {
    queue(key) {
      pending.set(JSON.stringify(key), key)
      if (timer) clearTimeout(timer)
      timer = setTimeout(flush, DEBOUNCE_MS)
    },
    dispose() {
      if (timer) clearTimeout(timer)
      timer = null
      pending.clear()
    },
  }
}
