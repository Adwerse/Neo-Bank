import { Banner } from '../ui/Banner'
import { Button } from '../ui/Button'
import { useConnectionStatus } from './WebSocketProvider'
import styles from './ConnectionStatus.module.css'

// Three states, matching the product requirement exactly: connected shows
// nothing (normal doesn't need attention), a fresh reconnect attempt is a
// thin non-blocking strip, and a reconnect that's actually taking a while
// (isStale) is the only one that gets a real banner + manual retry. Never
// a modal — the app is fully usable on fallback polling the whole time
// (see useMe/useOperationHistory/useDepositStatusPolling), so this is a
// convenience degradation, not an outage screen.
export function ConnectionStatus() {
  const { wsState, isStale, refreshNow } = useConnectionStatus()

  if (isStale) {
    return (
      <Banner variant="warning" className={styles.staleBanner}>
        <span>Нет соединения в реальном времени — данные могут быть неактуальны.</span>
        <Button type="button" variant="secondary" className={styles.refreshButton} onClick={refreshNow}>
          Обновить
        </Button>
      </Banner>
    )
  }

  if (wsState === 'reconnecting') {
    return (
      <div className={styles.reconnectingBar} role="status" aria-live="polite">
        Переподключение… обновления могут запаздывать
      </div>
    )
  }

  return null
}
