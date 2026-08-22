import { Link, NavLink } from 'react-router'
import { getAccessTokenEmail } from '../shared/api-client/jwt'
import { ArrowUpRightIcon, CardIcon, HomeIcon, PlusCircleIcon } from '../shared/ui/icons'
import { getDisplayName } from '../features/accounts/displayName'
import styles from './Sidebar.module.css'

const navItems = [
  { to: '/dashboard', label: 'Главная', Icon: HomeIcon },
  { to: '/transfers', label: 'Переводы', Icon: ArrowUpRightIcon },
  { to: '/deposit', label: 'Пополнение', Icon: PlusCircleIcon },
]

// No Настройки item — no route or page exists for it. Реквизиты (below)
// isn't a NavLink like the three above: it jumps to a section of /dashboard
// rather than naming "the page you're on," so it never carries the active
// highlight (which would otherwise also light up whenever Главная is
// active, since both resolve to the same /dashboard pathname).
export function Sidebar() {
  const { name, initial } = getDisplayName(getAccessTokenEmail())

  return (
    <aside className={styles.sidebar}>
      <div className={styles.top}>
        <div className={styles.wordmark}>NEO·BANK</div>
      </div>

      <nav className={styles.nav}>
        {navItems.map(({ to, label, Icon }) => (
          <NavLink
            key={to}
            to={to}
            end
            className={({ isActive }) => [styles.navItem, isActive && styles.navItemActive].filter(Boolean).join(' ')}
          >
            <Icon size={16} />
            {label}
          </NavLink>
        ))}
        <Link to="/dashboard#account-details" className={styles.navItem}>
          <CardIcon size={16} />
          Реквизиты
        </Link>
      </nav>

      <div className={styles.spacer} />

      <div className={styles.footer}>
        <div className={styles.avatar}>{initial}</div>
        <div className={styles.footerText}>
          <div className={styles.name}>{name}</div>
          {/* Session presence (always true while this is mounted — Sidebar
              only renders for an authenticated session), not the bank
              account's own frozen/active/closed status, which is shown
              separately on the dashboard. */}
          <div className={styles.sessionStatus}>● Активен</div>
        </div>
      </div>
    </aside>
  )
}
