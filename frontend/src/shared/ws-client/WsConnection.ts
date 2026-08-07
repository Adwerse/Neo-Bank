import { nextReconnectDelayMs } from './backoff'

export type WsConnectionState = 'idle' | 'connecting' | 'open' | 'reconnecting'

export interface WsConnectionOptions {
  url: string
  // Read fresh on every connect attempt (not captured once) — a reconnect
  // that follows a token refresh elsewhere in the app must pick up the new
  // token, not keep retrying with the one that was current when connect()
  // was first called.
  getToken: () => string | null
  // Fires only when a connection attempt actually reached the server (the
  // WebSocket handshake completed, so the auth message was sent) and was
  // then closed without ever getting `{"type":"connected"}` back — i.e. the
  // token itself was rejected, not "the network/gateway was unreachable".
  // Access tokens are short-lived (15min, see gateway/ws.go) while this
  // connection is meant to survive for hours, so this is the expected way
  // an old token surfaces. Awaited before the next attempt so a caller can
  // refresh the token (see WebSocketProvider's use of bootstrapSession)
  // instead of retrying with the same one forever.
  //
  // Deliberately NOT fired when the handshake itself never completed
  // (connection refused, DNS failure, a Gateway that's mid-restart) — that
  // case is indistinguishable from "briefly offline" and must not be
  // treated as an auth problem. bootstrapSession() on a real 401 clears
  // tokens and hard-redirects to /login on failure (see
  // shared/api-client/client.ts); firing it here on ordinary connectivity
  // loss would silently log the user out the moment a Gateway restart
  // (exactly the scenario this module exists to survive) made one
  // reconnect attempt land before the server was accepting connections
  // again.
  onAuthFailure?: () => Promise<void>
  // Fires after every successful (re)connect, including the first one.
  onOpen?: () => void
  onMessage?: (message: unknown) => void
  onStateChange?: (state: WsConnectionState) => void
}

// Manages one logical WebSocket connection to the Gateway: dials, sends the
// required auth-first-message handshake, and — for any close that wasn't
// requested via disconnect() — reconnects with jittered exponential
// backoff (see backoff.ts). Framework-agnostic on purpose: WebSocketProvider
// is what ties this to React (auth state, react-query, Page Visibility).
export class WsConnection {
  private socket: WebSocket | null = null
  private state: WsConnectionState = 'idle'
  private attempt = 0
  private socketOpened = false
  private authAcked = false
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null
  private intentionallyClosed = true
  private readonly options: WsConnectionOptions

  constructor(options: WsConnectionOptions) {
    this.options = options
  }

  // Idempotent: calling connect() while already connecting/open/reconnecting
  // is a no-op rather than tearing down and redialing.
  connect(): void {
    if (!this.intentionallyClosed) return
    this.intentionallyClosed = false
    this.clearReconnectTimer()
    this.open()
  }

  // The only path that stops reconnect attempts for good. Anything else
  // that closes the socket (server restart, network loss, auth rejection)
  // is treated as unintentional and scheduled for retry.
  disconnect(): void {
    this.intentionallyClosed = true
    this.clearReconnectTimer()
    this.attempt = 0
    this.socket?.close(1000, 'client disconnect')
    this.socket = null
    this.setState('idle')
  }

  private open(): void {
    const token = this.options.getToken()
    if (!token) {
      // No token to authenticate with right now (e.g. auth state flipped
      // mid-backoff). Don't spin on this — the caller drives connect()
      // again once a session exists (see WebSocketProvider's auth-status
      // effect).
      this.disconnect()
      return
    }

    this.socketOpened = false
    this.authAcked = false
    this.setState(this.attempt === 0 ? 'connecting' : 'reconnecting')

    const socket = new WebSocket(this.options.url)
    this.socket = socket

    socket.onopen = () => {
      this.socketOpened = true
      // Auth travels as the first message, never a query param — a WS URL
      // (unlike a fetch Authorization header) is the kind of thing that
      // ends up in proxy/server access logs.
      socket.send(JSON.stringify({ type: 'auth', token }))
    }

    socket.onmessage = (event) => {
      let data: unknown
      try {
        data = JSON.parse(event.data as string)
      } catch {
        return
      }
      if (!this.authAcked) {
        if (isConnectedAck(data)) {
          this.authAcked = true
          this.attempt = 0
          this.setState('open')
          this.options.onOpen?.()
        }
        return
      }
      this.options.onMessage?.(data)
    }

    socket.onclose = () => {
      this.socket = null
      if (this.intentionallyClosed) {
        this.setState('idle')
        return
      }
      this.scheduleReconnect()
    }
  }

  private scheduleReconnect(): void {
    this.setState('reconnecting')
    // Only "handshake completed but no ack" counts as a rejected token —
    // a handshake that never completed at all means the server (or
    // network) was simply unreachable, not that it said no.
    const wasRejected = this.socketOpened && !this.authAcked
    const delay = nextReconnectDelayMs(this.attempt)
    this.attempt += 1
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null
      void this.retry(wasRejected)
    }, delay)
  }

  private async retry(wasRejected: boolean): Promise<void> {
    if (this.intentionallyClosed) return
    if (wasRejected && this.options.onAuthFailure) {
      try {
        await this.options.onAuthFailure()
      } catch {
        // A failed refresh attempt just means we retry with whatever
        // token is available next — same outcome as not having a hook.
      }
      if (this.intentionallyClosed) return
    }
    this.open()
  }

  private clearReconnectTimer(): void {
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer)
      this.reconnectTimer = null
    }
  }

  private setState(state: WsConnectionState): void {
    this.state = state
    this.options.onStateChange?.(state)
  }

  getState(): WsConnectionState {
    return this.state
  }
}

function isConnectedAck(data: unknown): boolean {
  return typeof data === 'object' && data !== null && (data as { type?: unknown }).type === 'connected'
}
