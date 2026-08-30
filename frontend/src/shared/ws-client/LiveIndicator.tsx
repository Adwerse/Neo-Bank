import { useWsConnected } from './WebSocketProvider'
import styles from './LiveIndicator.module.css'

// The copy changes with connection state, not just the dot — it must never
// claim real-time delivery while the app has actually fallen back to
// polling (see useMe/useDashboardOperations's FALLBACK_POLL_INTERVAL_MS).
export function LiveIndicator() {
  const connected = useWsConnected()
  return (
    <div className={styles.pill}>
      <span className={[styles.dot, connected ? styles.dotLive : styles.dotPolling].join(' ')} />
      <span>{connected ? 'Live updates' : 'Auto-refreshing every 5s'}</span>
    </div>
  )
}
