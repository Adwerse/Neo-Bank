import type { ReactNode } from 'react'
import { useLocation } from 'react-router'
import styles from './PageTransition.module.css'

// Wraps whatever a shell renders in place of <Outlet/>. Keying on pathname
// forces a fresh element on every route change (not on in-place re-renders
// of the same route), which re-triggers the CSS animation below — no
// routing library transition hook needed for a plain fade+settle.
export function PageTransition({ children }: { children: ReactNode }) {
  const { pathname } = useLocation()
  return (
    <div key={pathname} className={styles.page}>
      {children}
    </div>
  )
}
