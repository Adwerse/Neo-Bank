import { createContext, useContext, useEffect, useRef } from 'react'
import type { ReactNode } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { useAuth } from '../../features/auth/AuthContext'
import { bootstrapSession } from '../api-client/client'
import * as tokenStore from '../api-client/tokenStore'
import { WsConnection } from './WsConnection'
import { getWsUrl } from './wsUrl'

type WsMessageListener = (message: unknown) => void

interface WebSocketContextValue {
  // Registers a listener for every non-handshake message the Gateway
  // pushes (see gateway/notify.go for the current set of `type`s). Nothing
  // in this module acts on message content yet — reacting to specific
  // types is the next step; this is just the wiring for it.
  subscribe: (listener: WsMessageListener) => () => void
}

const WebSocketContext = createContext<WebSocketContextValue | null>(null)

export function WebSocketProvider({ children }: { children: ReactNode }) {
  const { status } = useAuth()
  const queryClient = useQueryClient()
  const listenersRef = useRef(new Set<WsMessageListener>())

  useEffect(() => {
    if (status !== 'authenticated') return

    // A WebSocket is not a durable queue: whatever happened on the server
    // while this connection was down (Gateway restart, the user's laptop
    // sleeping, a dead wifi network) produced events nobody will ever
    // resend. The only way to not silently show stale data after any of
    // that is to distrust the cache the moment a connection comes back and
    // pull current state over HTTP — regardless of which message (if any)
    // shows up next. Same two keys TransferForm/useDepositStatusPolling
    // already invalidate after a known-relevant mutation.
    function refetchAfterReconnect() {
      queryClient.invalidateQueries({ queryKey: ['accounts', 'me'] })
      queryClient.invalidateQueries({ queryKey: ['transfers', 'history'] })
    }

    const connection = new WsConnection({
      url: getWsUrl(),
      getToken: tokenStore.getAccessToken,
      onAuthFailure: async () => {
        await bootstrapSession()
      },
      onOpen: refetchAfterReconnect,
      onMessage: (message) => {
        for (const listener of listenersRef.current) listener(message)
      },
    })
    connection.connect()

    function handleVisibilityChange() {
      if (document.visibilityState === 'hidden') {
        connection.disconnect()
      } else {
        // connect() re-dials, which re-authenticates and (once the server
        // acks) re-triggers refetchAfterReconnect — a tab that was
        // backgrounded is exactly the "missed events" case task 3 exists
        // for, so it must not skip that refetch just because the pause was
        // deliberate rather than a network failure.
        connection.connect()
      }
    }
    document.addEventListener('visibilitychange', handleVisibilityChange)

    return () => {
      document.removeEventListener('visibilitychange', handleVisibilityChange)
      connection.disconnect()
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
  }

  return <WebSocketContext.Provider value={value}>{children}</WebSocketContext.Provider>
}

export function useWsSubscription(listener: WsMessageListener): void {
  const ctx = useContext(WebSocketContext)
  if (!ctx) {
    throw new Error('useWsSubscription must be used within a WebSocketProvider')
  }
  useEffect(() => ctx.subscribe(listener), [ctx, listener])
}
