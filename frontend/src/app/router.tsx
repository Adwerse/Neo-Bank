import { createBrowserRouter, Navigate } from 'react-router'
import { Layout } from './Layout'
import { LoginPage } from '../features/auth/components/LoginPage'
import { RegisterPage } from '../features/auth/components/RegisterPage'
import { VerifyEmailPage } from '../features/auth/components/VerifyEmailPage'
import { ForgotPasswordPage } from '../features/auth/components/ForgotPasswordPage'
import { ResetPasswordPage } from '../features/auth/components/ResetPasswordPage'
import { RequireAuth, RequireGuest } from '../features/auth/components/RouteGuards'
import { DashboardPage } from '../features/accounts/components/DashboardPage'
import { TransfersPage } from '../features/transfers/components/TransfersPage'
import { DepositPage } from '../features/deposits/components/DepositPage'

export const router = createBrowserRouter([
  {
    element: <Layout />,
    children: [
      { index: true, element: <Navigate to="/login" replace /> },
      {
        path: 'login',
        element: (
          <RequireGuest>
            <LoginPage />
          </RequireGuest>
        ),
      },
      {
        path: 'register',
        element: (
          <RequireGuest>
            <RegisterPage />
          </RequireGuest>
        ),
      },
      { path: 'verify-email', element: <VerifyEmailPage /> },
      { path: 'forgot-password', element: <ForgotPasswordPage /> },
      { path: 'reset-password', element: <ResetPasswordPage /> },
      {
        path: 'dashboard',
        element: (
          <RequireAuth>
            <DashboardPage />
          </RequireAuth>
        ),
      },
      {
        path: 'transfers',
        element: (
          <RequireAuth>
            <TransfersPage />
          </RequireAuth>
        ),
      },
      {
        path: 'deposit',
        element: (
          <RequireAuth>
            <DepositPage />
          </RequireAuth>
        ),
      },
    ],
  },
])
