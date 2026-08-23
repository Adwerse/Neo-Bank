import { useQuery } from '@tanstack/react-query'
import { useWsConnected } from '../../shared/ws-client/WebSocketProvider'
import { getBalanceHistory, type BalanceHistoryRange } from './api'

// Same fallback cadence and rationale as useMe.ts.
const FALLBACK_POLL_INTERVAL_MS = 5000

export function useBalanceHistory(range: BalanceHistoryRange) {
  const wsConnected = useWsConnected()
  return useQuery({
    queryKey: ['accounts', 'balance-history', range],
    queryFn: () => getBalanceHistory(range),
    retry: false,
    refetchInterval: wsConnected ? false : FALLBACK_POLL_INTERVAL_MS,
  })
}
