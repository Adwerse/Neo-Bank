import type { ReactNode } from 'react'
import { Navigate } from 'react-router'
import { useAuth } from '../AuthContext'

// status === 'loading' renders nothing rather than redirecting — otherwise
// the session-bootstrap check (an in-flight /auth/refresh on page load)
// would get misread as "logged out" before it resolves.

export function RequireAuth({ children }: { children: ReactNode }) {
  const { status } = useAuth()
  if (status === 'loading') return null
  if (status === 'unauthenticated') return <Navigate to="/login" replace />
  return <>{children}</>
}

export function RequireGuest({ children }: { children: ReactNode }) {
  const { status } = useAuth()
  if (status === 'loading') return null
  if (status === 'authenticated') return <Navigate to="/dashboard" replace />
  return <>{children}</>
}
