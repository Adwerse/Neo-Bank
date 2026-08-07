// Same-origin, same-host-and-port as the page itself — matching how
// shared/api-client/client.ts calls "/api/*" through the Vite dev proxy
// (see vite.config.ts) rather than dialing the Gateway directly. That
// proxy has a "/ws" entry alongside "/api" for the same reason: keep the
// browser talking to one origin in dev, no CORS/websocket-origin story to
// solve separately for production either, since the Gateway is expected
// to sit behind the same host there too.
export function getWsUrl(): string {
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${protocol}//${window.location.host}/ws`
}
