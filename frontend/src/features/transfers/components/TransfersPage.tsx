import { TransferForm } from './TransferForm'
import { TransferHistory } from './TransferHistory'
import styles from './TransfersPage.module.css'

export function TransfersPage() {
  return (
    <div className={styles.page}>
      <TransferForm />
      <TransferHistory />
    </div>
  )
}
