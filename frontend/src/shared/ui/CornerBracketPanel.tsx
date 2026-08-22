import type { ReactNode } from 'react'
import styles from './CornerBracketPanel.module.css'

// Bordered/shadowed wrapper with 4 dimension-line-style corner brackets —
// the Blueprint dashboard's signature "technical drawing" decoration,
// wrapping the balance+chart panel.
export function CornerBracketPanel({ children }: { children: ReactNode }) {
  return (
    <div className={styles.panel}>
      <span className={`${styles.corner} ${styles.topLeft}`} aria-hidden="true" />
      <span className={`${styles.corner} ${styles.topRight}`} aria-hidden="true" />
      <span className={`${styles.corner} ${styles.bottomLeft}`} aria-hidden="true" />
      <span className={`${styles.corner} ${styles.bottomRight}`} aria-hidden="true" />
      {children}
    </div>
  )
}
