import { ApiError } from './ApiError'
import * as tokenStore from './tokenStore'
import type { paths } from './schema'

const BASE_URL = '/api'

type TokenPair = paths['/auth/refresh']['post']['responses']['200']['content']['application/json']

export interface RequestOptions {
  // Set by endpoints that either run before a session exists (register,
  // login, ...) or whose own 401 is a business answer rather than an
  // "access token expired" signal (e.g. /auth/login: wrong password, not a
  // stale token) — those must never trigger the refresh-and-retry flow
  // below.
  skipAuthRetry?: boolean
}

export async function request<T>(
  path: string,
  init: RequestInit = {},
  options: RequestOptions = {},
): Promise<T> {
  const res = await rawFetch(path, init)
  if (res.ok) {
    return parseBody<T>(res)
  }

  if (res.status === 401 && !options.skipAuthRetry && tokenStore.getRefreshToken()) {
    await getOrStartRefresh()
    const retryRes = await rawFetch(path, init)
    if (retryRes.ok) {
      return parseBody<T>(retryRes)
    }
    throw await toApiError(retryRes)
  }

  throw await toApiError(res)
}

function rawFetch(path: string, init: RequestInit): Promise<Response> {
  const headers = new Headers(init.headers)
  if (init.body !== undefined) {
    headers.set('Content-Type', 'application/json')
  }
  const accessToken = tokenStore.getAccessToken()
  if (accessToken) {
    headers.set('Authorization', `Bearer ${accessToken}`)
  }
  return fetch(`${BASE_URL}${path}`, { ...init, headers })
}

async function parseBody<T>(res: Response): Promise<T> {
  if (res.status === 204) {
    return undefined as T
  }
  const text = await res.text()
  return (text ? JSON.parse(text) : undefined) as T
}

async function toApiError(res: Response): Promise<ApiError> {
  let body: unknown
  try {
    body = await parseBody(res)
  } catch {
    body = undefined
  }
  const message =
    body && typeof body === 'object' && 'error' in body && typeof (body as { error: unknown }).error === 'string'
      ? (body as { error: string }).error
      : undefined
  return new ApiError(res.status, body, message)
}

// --- single-flight refresh ---
//
// Every request that hits a 401 independently calls getOrStartRefresh().
// The FIRST caller creates refreshPromise and actually performs the
// POST /auth/refresh; every other concurrent caller sees refreshPromise
// already set and awaits that same promise instead of firing its own.
//
// This isn't just an optimization: refresh tokens are single-use/rotating
// (sprint 1). If N requests expiring around the same moment each started
// their own refresh, only the first would succeed — the other N-1 would
// each try to redeem a refresh token some earlier call already consumed,
// get rejected, and wrongly log the user out. Sharing one in-flight
// promise across all of them is what makes that not happen.
let refreshPromise: Promise<string> | null = null

function getOrStartRefresh(): Promise<string> {
  if (!refreshPromise) {
    refreshPromise = performRefresh().finally(() => {
      refreshPromise = null
    })
  }
  return refreshPromise
}

async function performRefresh(): Promise<string> {
  const refreshToken = tokenStore.getRefreshToken()
  if (!refreshToken) {
    tokenStore.clearTokens()
    redirectToLogin()
    throw new ApiError(401, null, 'no refresh token available')
  }

  const res = await fetch(`${BASE_URL}/auth/refresh`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ refresh_token: refreshToken }),
  })

  if (!res.ok) {
    // An authoritative rejection from the refresh endpoint itself (invalid,
    // expired, or already-used token; suspended account) is the one case
    // that genuinely means the session is over. A network failure to even
    // reach this endpoint (below) is deliberately handled differently.
    const err = await toApiError(res)
    tokenStore.clearTokens()
    redirectToLogin()
    throw err
  }

  const pair = await parseBody<TokenPair>(res)
  tokenStore.setTokenPair(pair)
  return pair.access_token
}

function redirectToLogin(): void {
  if (typeof window !== 'undefined') {
    window.location.href = '/login'
  }
}
