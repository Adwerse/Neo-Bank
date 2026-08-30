import { Outlet } from 'react-router'
import { getAccessTokenEmail } from '../shared/api-client/jwt'
import { getDisplayName } from '../features/accounts/displayName'
import { APP_LOCALE } from '../shared/locale'
import { LiveIndicator } from '../shared/ws-client/LiveIndicator'
import { PageTransition } from '../shared/ui/PageTransition'
import { Sidebar } from './Sidebar'
import styles from './DesktopShell.module.css'

const dateFormat = new Intl.DateTimeFormat(APP_LOCALE, { day: '2-digit', month: '2-digit', year: 'numeric' })
const weekdayFormat = new Intl.DateTimeFormat(APP_LOCALE, { weekday: 'long' })

export function DesktopShell() {
  const { name } = getDisplayName(getAccessTokenEmail())
  const now = new Date()
  const dateLabel = `${dateFormat.format(now)} · ${weekdayFormat.format(now)}`

  return (
    <div className={styles.shell}>
      <Sidebar />
      <div className={styles.content}>
        <div className={styles.header}>
          <div>
            <div className={styles.greeting}>Welcome back, {name}</div>
            <div className={styles.date}>{dateLabel}</div>
          </div>
          <LiveIndicator />
        </div>
        {/* /transfers and /deposit render their own unchanged content here
            too — this shell only supplies chrome (sidebar + header +
            background), never Blueprint-specific markup. .body centers
            whatever Outlet renders: a narrow Card (Transfers/Deposit) ends
            up centered like it always was under the old top-bar Layout,
            while BlueprintDashboard's own width:100% fills the row instead
            (and self-centers past its own 1400px cap on very wide
            screens). */}
        <div className={styles.body}>
          <PageTransition>
            <Outlet />
          </PageTransition>
        </div>
      </div>
    </div>
  )
}
