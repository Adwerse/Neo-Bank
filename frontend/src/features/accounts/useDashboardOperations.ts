import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { useWsConnected } from '../../shared/ws-client/WebSocketProvider'
import { listTransfers } from '../transfers/api'
import { computeRunningBalances } from './runningBalance'

// "Enough for a reasonable-looking recent-operations list and delta stat" —
// GET /transfers has no documented max, so this is a judgment call, not a
// guarantee. The balance-over-time chart no longer reads this (see
// BalanceChart.tsx/useBalanceHistory.ts, backed by a real ledger-svc
// aggregation instead) — this remains the operations table/list's and the
// balance-delta stat's data source.
const LIMIT = 100
// Same fallback cadence and reasoning as useMe's/useOperationHistory's —
// only used while the WS is down.
const FALLBACK_POLL_INTERVAL_MS = 5000

// Deliberately a separate query from useOperationHistory (not a
// parameterized version of it) so /transfers's existing query — and every
// WS-invalidation call site that targets it — is never put at risk. React
// Query v5 invalidates by key prefix by default, so
// ['transfers','history',{limit:100}] still gets live invalidation from
// every place that already calls invalidateQueries({queryKey:['transfers',
// 'history']}) (WebSocketProvider, TransferForm, useDepositStatusPolling) —
// zero changes needed there.
export function useDashboardOperations(currentBalance: number | undefined) {
  const wsConnected = useWsConnected()
  const query = useQuery({
    queryKey: ['transfers', 'history', { limit: LIMIT }],
    queryFn: () => listTransfers({ limit: LIMIT }),
    refetchInterval: wsConnected ? false : FALLBACK_POLL_INTERVAL_MS,
  })

  const derived = useMemo(() => {
    if (currentBalance === undefined || !query.data) {
      return { entries: [], balanceBeforeEarliest: currentBalance ?? 0 }
    }
    return computeRunningBalances(currentBalance, query.data)
  }, [currentBalance, query.data])

  return { ...query, entries: derived.entries, balanceBeforeEarliest: derived.balanceBeforeEarliest }
}
