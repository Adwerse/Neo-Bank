import { Link } from 'react-router'
import { ArrowDownLeftIcon, ArrowUpRightIcon, CardIcon, PlusCircleIcon } from '../../../shared/ui/icons'
import styles from './QuickActions.module.css'

interface QuickActionsProps {
  layout: 'row' | 'grid'
}

export function QuickActions({ layout }: QuickActionsProps) {
  return (
    <div className={layout === 'row' ? styles.row : styles.grid}>
      <Link to="/transfers" className={styles.action}>
        <ArrowUpRightIcon size={18} />
        Transfer
      </Link>
      <Link to="/deposit" className={styles.action}>
        <PlusCircleIcon size={18} />
        Top up
      </Link>
      {/* No withdrawal-initiation endpoint exists anywhere in the API —
          disabled, not omitted, so the 4-item layout rhythm holds and this
          reads as "not yet available" rather than a dead link. */}
      <button type="button" className={styles.action} disabled title="Not yet available">
        <ArrowDownLeftIcon size={18} />
        Withdraw
      </button>
      <Link to="/dashboard#account-details" className={styles.action}>
        <CardIcon size={18} />
        Account details
      </Link>
    </div>
  )
}
