import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { AuthProvider } from '../features/auth/AuthContext'
import { WebSocketProvider } from '../shared/ws-client/WebSocketProvider'

const queryClient = new QueryClient()

export function Providers({ children }: { children: ReactNode }) {
  return (
    <QueryClientProvider client={queryClient}>
      <AuthProvider>
        {/* Needs both useAuth() and useQueryClient(), so it must nest
            inside both providers above. */}
        <WebSocketProvider>{children}</WebSocketProvider>
      </AuthProvider>
    </QueryClientProvider>
  )
}
