import * as tokenStore from './tokenStore'

interface AccessTokenClaims {
  user_id: string
  email: string
}

// accounts-svc's /accounts/me response has no email field (it only knows
// the account, not the user) — but auth-svc's access token JWT already
// carries it (services/auth-svc/login.go's accessTokenClaims), so decoding
// the token already sitting in memory avoids a backend change entirely.
// This reads the payload only; it never needs to verify the signature since
// it's just for display, not an auth decision.
export function getAccessTokenEmail(): string | null {
  const token = tokenStore.getAccessToken()
  if (!token) return null
  try {
    const payload = token.split('.')[1]
    if (!payload) return null
    const claims = JSON.parse(base64UrlDecode(payload)) as Partial<AccessTokenClaims>
    return typeof claims.email === 'string' ? claims.email : null
  } catch {
    return null
  }
}

function base64UrlDecode(segment: string): string {
  const base64 = segment.replace(/-/g, '+').replace(/_/g, '/')
  const padded = base64.padEnd(base64.length + ((4 - (base64.length % 4)) % 4), '=')
  const bytes = Uint8Array.from(atob(padded), (c) => c.charCodeAt(0))
  return new TextDecoder().decode(bytes)
}
