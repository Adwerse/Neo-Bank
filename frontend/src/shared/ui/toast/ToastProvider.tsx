import { createContext, useCallback, useContext, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import styles from './ToastProvider.module.css'

const AUTO_DISMISS_MS = 6000

export type ToastVariant = 'info' | 'success' | 'warning' | 'danger'

interface Toast {
  id: number
  message: string
  variant: ToastVariant
}

interface ToastContextValue {
  showToast: (message: string, variant?: ToastVariant) => void
}

const ToastContext = createContext<ToastContextValue | null>(null)

// A stack of transient, dismissible messages for events that happen while
// the app is open and the user isn't necessarily looking at the one screen
// that would otherwise show them (an incoming transfer landing while the
// user is on /deposit, say) — see IncomingTransferWatcher.tsx. Deliberately
// not a browser/OS push notification: this only ever fires from inside a
// live app session, see WebSocketProvider's doc comments for why that's a
// separate, out-of-scope concern.
export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])
  const nextIdRef = useRef(0)

  const dismiss = useCallback((id: number) => {
    setToasts((current) => current.filter((toast) => toast.id !== id))
  }, [])

  const showToast = useCallback(
    (message: string, variant: ToastVariant = 'info') => {
      const id = nextIdRef.current++
      setToasts((current) => [...current, { id, message, variant }])
      setTimeout(() => dismiss(id), AUTO_DISMISS_MS)
    },
    [dismiss],
  )

  return (
    <ToastContext.Provider value={{ showToast }}>
      {children}
      <div className={styles.stack} role="region" aria-label="Notifications">
        {toasts.map((toast) => (
          <div
            key={toast.id}
            className={[styles.toast, styles[toast.variant]].join(' ')}
            role="status"
            aria-live="polite"
          >
            <span>{toast.message}</span>
            <button
              type="button"
              className={styles.dismissButton}
              onClick={() => dismiss(toast.id)}
              aria-label="Dismiss notification"
            >
              ×
            </button>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  )
}

export function useToast(): ToastContextValue {
  const ctx = useContext(ToastContext)
  if (!ctx) {
    throw new Error('useToast must be used within a ToastProvider')
  }
  return ctx
}
