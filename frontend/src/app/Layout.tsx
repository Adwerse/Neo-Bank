import { useEffect } from 'react'
import { NavLink, Outlet, useNavigate } from 'react-router'
import { useAuth } from '../features/auth/AuthContext'
import { ConnectionStatus } from '../shared/ws-client/ConnectionStatus'
import { IncomingTransferWatcher } from '../features/transfers/IncomingTransferWatcher'
import { useIsDesktop } from '../shared/ui/useIsDesktop'
import { DesktopShell } from './DesktopShell'
import styles from './Layout.module.css'

const navItems = [
  { to: '/login', label: 'Login' },
  { to: '/register', label: 'Register' },
  { to: '/dashboard', label: 'Dashboard' },
  { to: '/transfers', label: 'Transfers' },
  { to: '/deposit', label: 'Deposit' },
]

export function Layout() {
  const { status, logout } = useAuth()
  const navigate = useNavigate()
  const isDesktop = useIsDesktop()

  // Viewport width picks the theme now, not prefers-color-scheme — this is
  // the one place that decides it, so it must run regardless of which shell
  // ends up rendering below.
  useEffect(() => {
    document.documentElement.dataset.theme = isDesktop ? 'nocturne-light' : 'nocturne-dark'
  }, [isDesktop])

  async function handleLogout() {
    await logout()
    navigate('/login', { replace: true })
  }

  // The sidebar shell only makes sense for an authenticated session (its
  // nav and footer both need a logged-in user) — every other combination
  // (mobile at any auth state, desktop while logged out) keeps the top-bar
  // chrome below, just re-themed by the effect above.
  if (status === 'authenticated' && isDesktop) {
    return (
      <div className={styles.shell}>
        <ConnectionStatus />
        <IncomingTransferWatcher />
        <DesktopShell />
      </div>
    )
  }

  return (
    <div className={styles.shell}>
      <header className={styles.header}>
        <span className={styles.brand}>Neo-Bank</span>
        <nav className={styles.nav}>
          {navItems.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              className={({ isActive }) => (isActive ? styles.navLinkActive : styles.navLink)}
            >
              {item.label}
            </NavLink>
          ))}
          {status === 'authenticated' && (
            <button type="button" className={styles.navButton} onClick={handleLogout}>
              Logout
            </button>
          )}
        </nav>
      </header>
      {status === 'authenticated' && (
        <>
          <ConnectionStatus />
          <IncomingTransferWatcher />
        </>
      )}
      <main className={styles.main}>
        <Outlet />
      </main>
    </div>
  )
}
