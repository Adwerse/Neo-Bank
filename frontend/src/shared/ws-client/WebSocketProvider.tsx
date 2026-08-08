import { createContext, useContext, useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { useAuth } from '../../features/auth/AuthContext'
import { bootstrapSession } from '../api-client/client'
import * as tokenStore from '../api-client/tokenStore'
import { createInvalidationBatcher } from './invalidationBatcher'
import { WsConnection, type WsConnectionState } from './WsConnection'
import { getWsUrl } from './wsUrl'

type WsMessageListener = (message: unknown) => void

// How long a connection is allowed to sit in 'reconnecting' before the UI
// stops treating it as a quick blip and tells the user their data may be
// stale — long enough that one ordinary retry (backoff starts at ~500ms)
// never trips it, short enough that "honest" (see ConnectionStatus.tsx)
// still means something.
const STALE_AFTER_MS = 10_000

interface WebSocketContextValue {
  // Registers a listener for every non-handshake message the Gateway
  // pushes (see gateway/notify.go for the current set of `type`s). Cache
  // invalidation for known types is handled internally (below); this is
  // for anything else a future feature wants to react to directly.
  subscribe: (listener: WsMessageListener) => () => void
  // True only while a connection is open and past the auth handshake
  // (WsConnection's 'open' state) — 'connecting'/'reconnecting'/'idle' all
  // read as false. Queries use this to fall back to polling on networks
  // that never let a WS connection through at all (corporate proxies, some
  // mobile carriers) instead of just going silent.
  isConnected: boolean
  // Raw state straight from WsConnection, for UI that needs to distinguish
  // 'reconnecting' (a live connection just dropped) from 'idle' (tab
  // hidden, or logged out) — see ConnectionStatus.tsx.
  wsState: WsConnectionState
  // True once 'reconnecting' has lasted longer than STALE_AFTER_MS without
  // reaching 'open' again. Distinct from isConnected/wsState so a brief
  // reconnect (the common case) never flashes an alarming banner — only a
  // reconnect that's actually taking a while does.
  isStale: boolean
  // Bypasses the batcher's debounce for a user-initiated "Обновить" click —
  // they asked for current data right now, not in 150ms.
  refreshNow: () => void
}

const WebSocketContext = createContext<WebSocketContextValue | null>(null)

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null
}

export function WebSocketProvider({ children }: { children: ReactNode }) {
  const { status } = useAuth()
  const queryClient = useQueryClient()
  const listenersRef = useRef(new Set<WsMessageListener>())
  const [isConnected, setIsConnected] = useState(false)
  const [wsState, setWsState] = useState<WsConnectionState>('idle')
  const [isStale, setIsStale] = useState(false)
  const staleTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    if (status !== 'authenticated') return

    // Collapses a burst of signals (a completed transfer alone pushes
    // balance.changed + transfer.updated as two frames — see
    // gateway/notify.go) into one invalidation pass per distinct query key,
    // instead of one invalidateQueries call per frame.
    const batcher = createInvalidationBatcher(queryClient)

    // A WebSocket is not a durable queue: whatever happened on the server
    // while this connection was down (Gateway restart, the user's laptop
    // sleeping, a dead wifi network) produced events nobody will ever
    // resend. The only way to not silently show stale data after any of
    // that is to distrust the cache the moment a connection comes back and
    // pull current state over HTTP — regardless of which message (if any)
    // shows up next.
    function invalidateAfterReconnect() {
      batcher.queue(['accounts', 'me'])
      batcher.queue(['transfers', 'history'])
    }

    // Messages carry no data on the wire by design (see gateway/notify.go)
    // — only enough of an id to know *what* to re-fetch, never a value to
    // write into the cache directly.
    function handleMessage(message: unknown) {
      if (isRecord(message)) {
        switch (message.type) {
          case 'balance.changed':
            batcher.queue(['accounts', 'me'])
            break
          case 'transfer.updated':
            // No per-transfer detail cache exists in this app (GET
            // /transfers is the only transfer-shaped query) — the history
            // list is the entire cache surface a transfer can go stale in.
            batcher.queue(['transfers', 'history'])
            break
          case 'deposit.updated':
            batcher.queue(['accounts', 'me'])
            if (typeof message.deposit_id === 'string') {
              batcher.queue(['deposits', message.deposit_id])
            }
            break
          default:
            break
        }
      }
      for (const listener of listenersRef.current) listener(message)
    }

    function clearStaleTimer() {
      if (staleTimerRef.current) {
        clearTimeout(staleTimerRef.current)
        staleTimerRef.current = null
      }
    }

    // Drives ConnectionStatus.tsx: 'open' clears any pending stale flag,
    // 'reconnecting' starts a one-shot timer that flips isStale after
    // STALE_AFTER_MS (cleared the moment it either reaches 'open' or
    // leaves 'reconnecting' some other way). 'connecting' (first dial) and
    // 'idle' (tab hidden or logged out) are deliberately not "stale" —
    // neither means an existing connection just failed to come back.
    function handleStateChange(state: WsConnectionState) {
      setIsConnected(state === 'open')
      setWsState(state)
      if (state === 'reconnecting') {
        if (!staleTimerRef.current) {
          staleTimerRef.current = setTimeout(() => {
            staleTimerRef.current = null
            setIsStale(true)
          }, STALE_AFTER_MS)
        }
        return
      }
      clearStaleTimer()
      setIsStale(false)
    }

    const connection = new WsConnection({
      url: getWsUrl(),
      getToken: tokenStore.getAccessToken,
      onAuthFailure: async () => {
        await bootstrapSession()
      },
      onOpen: invalidateAfterReconnect,
      onMessage: handleMessage,
      onStateChange: handleStateChange,
    })
    connection.connect()

    function handleVisibilityChange() {
      if (document.visibilityState === 'hidden') {
        connection.disconnect()
      } else {
        // connect() re-dials, which re-authenticates and (once the server
        // acks) re-triggers invalidateAfterReconnect — a tab that was
        // backgrounded is exactly the "missed events" case that exists
        // for, so it must not skip that refetch just because the pause was
        // deliberate rather than a network failure.
        connection.connect()
      }
    }
    document.addEventListener('visibilitychange', handleVisibilityChange)

    return () => {
      document.removeEventListener('visibilitychange', handleVisibilityChange)
      connection.disconnect()
      batcher.dispose()
      clearStaleTimer()
      setIsConnected(false)
      setWsState('idle')
      setIsStale(false)
    }
    // Re-runs on every auth transition, tearing down the previous
    // connection first: logout must not leave a connection open, and a
    // different user logging in afterward must get a brand new one rather
    // than inherit whichever socket the last user had.
  }, [status, queryClient])

  const value: WebSocketContextValue = {
    subscribe: (listener) => {
      listenersRef.current.add(listener)
      return () => listenersRef.current.delete(listener)
    },
    isConnected,
    wsState,
    isStale,
    refreshNow: () => {
      queryClient.invalidateQueries({ queryKey: ['accounts', 'me'] })
      queryClient.invalidateQueries({ queryKey: ['transfers', 'history'] })
    },
  }

  return <WebSocketContext.Provider value={value}>{children}</WebSocketContext.Provider>
}

function useWebSocketContext(): WebSocketContextValue {
  const ctx = useContext(WebSocketContext)
  if (!ctx) {
    throw new Error('useWebSocketContext must be used within a WebSocketProvider')
  }
  return ctx
}

export function useWsSubscription(listener: WsMessageListener): void {
  const ctx = useWebSocketContext()
  useEffect(() => ctx.subscribe(listener), [ctx, listener])
}

// Queries use this to switch their own refetchInterval between "off, WS
// covers it" and "on, WS isn't getting through" — see useMe,
// useOperationHistory, and useDepositStatusPolling.
export function useWsConnected(): boolean {
  return useWebSocketContext().isConnected
}

export interface WsConnectionStatus {
  wsState: WsConnectionState
  isStale: boolean
  refreshNow: () => void
}

// ConnectionStatus.tsx's data source — kept separate from useWsConnected
// (a plain boolean most queries only need) so components that render UI off
// the connection state pull exactly the three fields they need.
export function useConnectionStatus(): WsConnectionStatus {
  const ctx = useWebSocketContext()
  return { wsState: ctx.wsState, isStale: ctx.isStale, refreshNow: ctx.refreshNow }
}
