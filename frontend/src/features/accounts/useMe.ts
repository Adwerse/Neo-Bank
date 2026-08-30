import { useQuery } from '@tanstack/react-query'
import { useWsConnected } from '../../shared/ws-client/WebSocketProvider'
import { getMe } from './api'

// Fallback cadence for the networks a WS connection never gets through on
// (corporate proxies that strip Upgrade headers, some mobile carriers) —
// slow enough not to hammer the backend, fast enough that a stuck balance
// isn't a multi-minute wait behind it. Only used while the WS is down; see
// WebSocketProvider's balance.changed handler for the live path.
const FALLBACK_POLL_INTERVAL_MS = 5000

export function useMe() {
  const wsConnected = useWsConnected()
  return useQuery({
    queryKey: ['accounts', 'me'],
    queryFn: getMe,
    // A predictable manual "Retry" button beats silent background
    // retries on a balance screen — the user should see the failure and
    // choose to retry, not wait through a hidden backoff.
    retry: false,
    refetchInterval: wsConnected ? false : FALLBACK_POLL_INTERVAL_MS,
  })
}
