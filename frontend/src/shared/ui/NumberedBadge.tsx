import styles from './NumberedBadge.module.css'

interface NumberedBadgeProps {
  n: number
  label: string
}

// The circled-number section headers used throughout the Blueprint
// dashboard (①  Баланс, ②  Динамика, …) — the footer legend that spells out
// what each number means lives in BlueprintDashboard itself.
export function NumberedBadge({ n, label }: NumberedBadgeProps) {
  return (
    <div className={styles.badge}>
      <span className={styles.circle}>{n}</span>
      <span className={styles.label}>{label}</span>
    </div>
  )
}
