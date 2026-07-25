import { createContext, useContext, useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import { bootstrapSession } from '../../shared/api-client/client'
import * as tokenStore from '../../shared/api-client/tokenStore'
import { login as apiLogin, logout as apiLogout } from './api'

type AuthStatus = 'loading' | 'authenticated' | 'unauthenticated'

interface AuthContextValue {
  status: AuthStatus
  login: (email: string, password: string) => Promise<void>
  logout: () => Promise<void>
  clearSession: () => void
}

const AuthContext = createContext<AuthContextValue | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<AuthStatus>('loading')

  useEffect(() => {
    let cancelled = false
    // The access token is memory-only and never survives a reload — this is
    // what re-establishes a session from the stored refresh token on boot.
    bootstrapSession().then((authenticated) => {
      if (!cancelled) setStatus(authenticated ? 'authenticated' : 'unauthenticated')
    })
    return () => {
      cancelled = true
    }
  }, [])

  async function login(email: string, password: string) {
    const pair = await apiLogin({ email, password })
    tokenStore.setTokenPair(pair)
    setStatus('authenticated')
  }

  async function logout() {
    const refreshToken = tokenStore.getRefreshToken()
    try {
      if (refreshToken) {
        await apiLogout({ refresh_token: refreshToken })
      }
    } finally {
      // Logout must succeed locally even if the network call fails — the
      // user asked to end their session, and a flaky request shouldn't be
      // able to trap them in it.
      tokenStore.clearTokens()
      setStatus('unauthenticated')
    }
  }

  // For flows where the backend has already invalidated the session
  // server-side (password reset revokes every refresh token) — just brings
  // local state in line, no API call.
  function clearSession() {
    tokenStore.clearTokens()
    setStatus('unauthenticated')
  }

  return (
    <AuthContext.Provider value={{ status, login, logout, clearSession }}>{children}</AuthContext.Provider>
  )
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext)
  if (!ctx) {
    throw new Error('useAuth must be used within an AuthProvider')
  }
  return ctx
}
