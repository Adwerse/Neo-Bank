import { TransferForm } from './TransferForm'
import { OperationHistory } from './OperationHistory'
import styles from './TransfersPage.module.css'

export function TransfersPage() {
  return (
    <div className={styles.page}>
      <TransferForm />
      <OperationHistory />
    </div>
  )
}
