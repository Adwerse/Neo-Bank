import { useInfiniteQuery } from '@tanstack/react-query'
import { useWsConnected } from '../../shared/ws-client/WebSocketProvider'
import { listOperationHistoryPage } from './api'

// Same fallback cadence and reasoning as useMe's — only used while the WS
// is down; see WebSocketProvider's transfer.updated handler for the live
// path (a pending transfer resolving, including via the reconciliation
// worker, pushes this list stale the moment it happens). A refetch on an
// infinite query re-fetches every page already loaded (in sequence, by
// their original cursors), so a stuck WS still keeps every loaded page —
// not just the first — current.
const FALLBACK_POLL_INTERVAL_MS = 5000
const PAGE_SIZE = 20

// The query key deliberately stays exactly ['transfers', 'history'] (same
// as before this became an infinite query) — WebSocketProvider's
// transfer.updated/deposit.updated handlers invalidate that same key, and
// React Query matches invalidateQueries by prefix, so this must not grow
// extra key segments or WS-driven invalidation silently stops finding it.
export function useOperationHistory() {
  const wsConnected = useWsConnected()
  return useInfiniteQuery({
    queryKey: ['transfers', 'history'],
    queryFn: ({ pageParam }) => listOperationHistoryPage({ limit: PAGE_SIZE, cursor: pageParam }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (lastPage) => lastPage.next_cursor,
    refetchInterval: wsConnected ? false : FALLBACK_POLL_INTERVAL_MS,
  })
}
