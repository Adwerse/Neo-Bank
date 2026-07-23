const REFRESH_TOKEN_STORAGE_KEY = 'neobank.refreshToken'

// The access token lives only in memory (a module-level variable) and never
// touches localStorage. It doesn't survive a page reload — the refresh
// token below is what re-establishes a session after one, via a silent
// refresh. See the root README for the tradeoff this implies.
let accessToken: string | null = null

export function getAccessToken(): string | null {
  return accessToken
}

export function setAccessToken(token: string | null): void {
  accessToken = token
}

export function getRefreshToken(): string | null {
  return localStorage.getItem(REFRESH_TOKEN_STORAGE_KEY)
}

export function setRefreshToken(token: string | null): void {
  if (token) {
    localStorage.setItem(REFRESH_TOKEN_STORAGE_KEY, token)
  } else {
    localStorage.removeItem(REFRESH_TOKEN_STORAGE_KEY)
  }
}

export function setTokenPair(pair: { access_token: string; refresh_token: string }): void {
  setAccessToken(pair.access_token)
  setRefreshToken(pair.refresh_token)
}

export function clearTokens(): void {
  setAccessToken(null)
  setRefreshToken(null)
}
