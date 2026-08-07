import { useQuery } from '@tanstack/react-query'
import { useWsConnected } from '../../shared/ws-client/WebSocketProvider'
import { listTransfers } from './api'

// Same fallback cadence and reasoning as useMe's — only used while the WS
// is down; see WebSocketProvider's transfer.updated handler for the live
// path (a pending transfer resolving, including via the reconciliation
// worker, pushes this list stale the moment it happens).
const FALLBACK_POLL_INTERVAL_MS = 5000

export function useOperationHistory() {
  const wsConnected = useWsConnected()
  return useQuery({
    queryKey: ['transfers', 'history'],
    queryFn: () => listTransfers({ limit: 20 }),
    refetchInterval: wsConnected ? false : FALLBACK_POLL_INTERVAL_MS,
  })
}
