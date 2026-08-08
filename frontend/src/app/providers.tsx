import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { AuthProvider } from '../features/auth/AuthContext'
import { WebSocketProvider } from '../shared/ws-client/WebSocketProvider'
import { ToastProvider } from '../shared/ui/toast/ToastProvider'

const queryClient = new QueryClient()

export function Providers({ children }: { children: ReactNode }) {
  return (
    <QueryClientProvider client={queryClient}>
      <AuthProvider>
        {/* Needs both useAuth() and useQueryClient(), so it must nest
            inside both providers above. */}
        <WebSocketProvider>
          {/* IncomingTransferWatcher (rendered inside Layout, a child of
              children below) needs useToast(), so ToastProvider must wrap
              it too. */}
          <ToastProvider>{children}</ToastProvider>
        </WebSocketProvider>
      </AuthProvider>
    </QueryClientProvider>
  )
}
