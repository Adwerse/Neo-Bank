import { useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useWsConnected } from '../../shared/ws-client/WebSocketProvider'
import { getDeposit } from './api'

// A successful client-side confirmPayment only means Stripe accepted the
// charge (deposit status 'succeeded') — the ledger balance doesn't move
// until the background reconciliation worker credits it (status
// 'credited'), which runs on a fixed 30s tick
// (services/transfers-svc/reconcile.go's reconcileInterval). The Gateway
// pushes deposit.updated the moment that happens (WebSocketProvider
// invalidates this exact query key on it), so POLL_INTERVAL_MS below is
// only a fallback for a WS connection that never gets through at all —
// 2s catches the reconcile-worker transition promptly without hammering
// the backend in that mode. GIVE_UP_AFTER_MS (60s, 2x the worst-case
// reconcile tick) bounds how long the UI shows a spinner either way,
// independent of whether updates are arriving via WS or the fallback poll.
const POLL_INTERVAL_MS = 2000
const GIVE_UP_AFTER_MS = 60_000

// 'refunded' is only reachable from 'credited' (a charge.refunded event
// after this service already credited the deposit — see
// services/transfers-svc/webhook.go's processChargeRefundedEvent). In the
// ordinary case this hook stops polling the moment it sees 'credited' and
// never observes a later refund at all; the only way it could see
// 'refunded' directly is the rare race where both transitions land
// between two poll ticks. Treated the same as 'failed' here rather than
// as a distinct UI state — by the time this poller could see it, showing
// "credited" would already be wrong, but inventing a whole separate
// "credited then reversed" message for a case this unlikely isn't worth
// the complexity.
const TERMINAL_NON_CREDITED_STATUSES = new Set(['failed', 'refunded'])

export type DepositPollOutcome = 'polling' | 'credited' | 'failed' | 'timed_out'

// useDepositStatusPolling is the honest-UX core of the deposit flow: it
// never reports success until the deposit's own status says 'credited',
// no matter what Stripe's client-side confirmPayment already claimed. See
// DepositPage's doc comment for the full state machine this feeds into.
export function useDepositStatusPolling(depositId: string | null) {
  const queryClient = useQueryClient()
  const wsConnected = useWsConnected()
  const startedAtRef = useRef(Date.now())
  // Forces a re-render exactly at the give-up mark even if no poll tick
  // happens to land right then — without this, a component that isn't
  // re-rendered for some other reason could keep showing a spinner past
  // 60s until something else happens to trigger a render.
  const [, forceTick] = useState(0)

  useEffect(() => {
    startedAtRef.current = Date.now()
    const timer = setTimeout(() => forceTick((n) => n + 1), GIVE_UP_AFTER_MS)
    return () => clearTimeout(timer)
  }, [depositId])

  const query = useQuery({
    queryKey: ['deposits', depositId],
    queryFn: () => getDeposit(depositId!),
    enabled: !!depositId,
    refetchInterval: (q) => {
      const status = q.state.data?.status
      if (status === 'credited' || (status && TERMINAL_NON_CREDITED_STATUSES.has(status))) return false
      if (Date.now() - startedAtRef.current > GIVE_UP_AFTER_MS) return false
      // WS connected: deposit.updated drives the refetch instead — polling
      // on top of that would just be redundant traffic for the same event.
      if (wsConnected) return false
      return POLL_INTERVAL_MS
    },
  })

  const status = query.data?.status

  useEffect(() => {
    if (status !== 'credited') return
    // A deposit row now exists in the credited state — both the balance
    // and the history may have changed. Same two keys TransferForm
    // invalidates after a transfer (features/transfers/components/TransferForm.tsx).
    queryClient.invalidateQueries({ queryKey: ['accounts', 'me'] })
    queryClient.invalidateQueries({ queryKey: ['transfers', 'history'] })
  }, [status, queryClient])

  let outcome: DepositPollOutcome = 'polling'
  if (status === 'credited') outcome = 'credited'
  else if (status && TERMINAL_NON_CREDITED_STATUSES.has(status)) outcome = 'failed'
  else if (Date.now() - startedAtRef.current > GIVE_UP_AFTER_MS) outcome = 'timed_out'

  return { deposit: query.data, outcome }
}
