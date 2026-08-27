import { NavLink, Outlet, useNavigate } from 'react-router'
import { useAuth } from '../features/auth/AuthContext'
import { getAccessTokenEmail } from '../shared/api-client/jwt'
import { getDisplayName } from '../features/accounts/displayName'
import { ArrowUpRightIcon, BellIcon, HomeIcon, PlusCircleIcon } from '../shared/ui/icons'
import { LiveIndicator } from '../shared/ws-client/LiveIndicator'
import { PageTransition } from '../shared/ui/PageTransition'
import styles from './MobileShell.module.css'

// Same three destinations as Sidebar.tsx's persistent nav — Реквизиты is
// deliberately not a fourth tab, same reasoning as there: it jumps to a
// section of /dashboard rather than naming a page of its own, so it stays
// reachable from the dashboard's IBAN block and QuickActions instead.
const tabItems = [
  { to: '/dashboard', label: 'Главная', Icon: HomeIcon },
  { to: '/transfers', label: 'Переводы', Icon: ArrowUpRightIcon },
  { to: '/deposit', label: 'Пополнение', Icon: PlusCircleIcon },
]

// Persistent chrome for every authenticated mobile page — avatar/name/bell
// and the live-connection indicator used to live inside TerminalDashboard
// itself; they moved here so they're the same across /dashboard,
// /transfers, and /deposit instead of being dashboard-only, mirroring how
// DesktopShell's header isn't BlueprintDashboard's to own either.
export function MobileShell() {
  const { name, initial } = getDisplayName(getAccessTokenEmail())
  const { logout } = useAuth()
  const navigate = useNavigate()

  async function handleLogout() {
    await logout()
    navigate('/login', { replace: true })
  }

  return (
    <div className={styles.shell}>
      <header className={styles.header}>
        <div className={styles.identity}>
          <div className={styles.avatar}>{initial}</div>
          <div>
            <div className={styles.greetingSmall}>С возвращением,</div>
            <div className={styles.greetingName}>{name}</div>
          </div>
        </div>
        <div className={styles.headerActions}>
          <button type="button" className={styles.bellButton} aria-label="Уведомления">
            <BellIcon size={17} />
          </button>
          <button type="button" className={styles.logoutButton} onClick={handleLogout}>
            Выйти
          </button>
        </div>
      </header>

      <LiveIndicator />

      <div className={styles.body}>
        <PageTransition>
          <Outlet />
        </PageTransition>
      </div>

      <nav className={styles.tabBar}>
        {tabItems.map(({ to, label, Icon }) => (
          <NavLink
            key={to}
            to={to}
            end
            className={({ isActive }) => [styles.tab, isActive && styles.tabActive].filter(Boolean).join(' ')}
          >
            <Icon size={20} />
            <span className={styles.tabLabel}>{label}</span>
          </NavLink>
        ))}
      </nav>
    </div>
  )
}
