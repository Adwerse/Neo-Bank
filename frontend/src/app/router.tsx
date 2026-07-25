import { createBrowserRouter, Navigate } from 'react-router'
import { Layout } from './Layout'
import { LoginPage } from '../features/auth/components/LoginPage'
import { RegisterPage } from '../features/auth/components/RegisterPage'
import { VerifyEmailPage } from '../features/auth/components/VerifyEmailPage'
import { DashboardPage } from '../features/accounts/components/DashboardPage'

export const router = createBrowserRouter([
  {
    element: <Layout />,
    children: [
      { index: true, element: <Navigate to="/login" replace /> },
      { path: 'login', element: <LoginPage /> },
      { path: 'register', element: <RegisterPage /> },
      { path: 'verify-email', element: <VerifyEmailPage /> },
      { path: 'dashboard', element: <DashboardPage /> },
    ],
  },
])
