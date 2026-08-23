import { useEffect } from 'react'
import { NavLink, Outlet } from 'react-router'
import { useAuth } from '../features/auth/AuthContext'
import { ConnectionStatus } from '../shared/ws-client/ConnectionStatus'
import { IncomingTransferWatcher } from '../features/transfers/IncomingTransferWatcher'
import { useIsDesktop } from '../shared/ui/useIsDesktop'
import { DesktopShell } from './DesktopShell'
import { MobileShell } from './MobileShell'
import styles from './Layout.module.css'

const navItems = [
  { to: '/login', label: 'Login' },
  { to: '/register', label: 'Register' },
  { to: '/dashboard', label: 'Dashboard' },
  { to: '/transfers', label: 'Transfers' },
  { to: '/deposit', label: 'Deposit' },
]

export function Layout() {
  const { status } = useAuth()
  const isDesktop = useIsDesktop()

  // Viewport width picks the theme now, not prefers-color-scheme — this is
  // the one place that decides it, so it must run regardless of which shell
  // ends up rendering below.
  useEffect(() => {
    document.documentElement.dataset.theme = isDesktop ? 'nocturne-light' : 'nocturne-dark'
  }, [isDesktop])

  // Both persistent-nav shells only make sense for an authenticated session
  // (their nav/header both need a logged-in user) — logged-out at any
  // width keeps the top-bar chrome below, just re-themed by the effect
  // above.
  if (status === 'authenticated' && isDesktop) {
    return (
      <div className={styles.shell}>
        <ConnectionStatus />
        <IncomingTransferWatcher />
        <DesktopShell />
      </div>
    )
  }

  if (status === 'authenticated') {
    return (
      <div className={styles.shell}>
        <ConnectionStatus />
        <IncomingTransferWatcher />
        <MobileShell />
      </div>
    )
  }

  // Reached only while logged out (both authenticated branches above
  // already returned) — no Logout button, ConnectionStatus, or
  // IncomingTransferWatcher, since none of those mean anything without a
  // session.
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
        </nav>
      </header>
      <main className={styles.main}>
        <Outlet />
      </main>
    </div>
  )
}
