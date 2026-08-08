import { useEffect, useRef } from 'react'
import { useToast } from '../../shared/ui/toast/ToastProvider'
import { formatMoney } from '../accounts/money'
import { useOperationHistory } from './useOperationHistory'
import type { OperationHistoryEntry } from './api'

function isIncomingCompleted(entry: OperationHistoryEntry): boolean {
  return entry.type === 'transfer' && entry.direction === 'incoming' && entry.status === 'completed'
}

// Renders nothing — it exists purely to keep ['transfers','history'] active
// (and therefore diffable) for the whole authenticated session, not just
// while TransfersPage happens to be mounted, so an incoming transfer that
// lands while the user is looking at /dashboard or /deposit still surfaces
// a toast instead of only silently updating a list nobody's viewing.
// Mounted once in Layout for authenticated users, same lifecycle as
// ConnectionStatus.
export function IncomingTransferWatcher() {
  const { data } = useOperationHistory()
  const { showToast } = useToast()
  const seenRef = useRef<Set<string> | null>(null)

  useEffect(() => {
    if (!data) return
    const seen = seenRef.current
    const currentlyIncoming = data.filter(isIncomingCompleted)
    if (seen) {
      for (const entry of currentlyIncoming) {
        if (!seen.has(entry.id)) {
          // No counterparty detail beyond what OperationHistory itself
          // already shows (see TransfersPage) — this is a "something
          // happened, go look" nudge, not a second place to read details.
          showToast(`Входящий перевод: +${formatMoney(entry.amount, 'EUR')}`)
        }
      }
    }
    seenRef.current = new Set(currentlyIncoming.map((entry) => entry.id))
  }, [data, showToast])

  return null
}
