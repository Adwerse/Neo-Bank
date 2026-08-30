import { useDocumentTitle } from '../../../shared/ui/useDocumentTitle'
import { TransferForm } from './TransferForm'
import { OperationHistory } from './OperationHistory'
import styles from './TransfersPage.module.css'

export function TransfersPage() {
  useDocumentTitle('Transfers')
  return (
    <div className={styles.page}>
      <TransferForm />
      <OperationHistory />
    </div>
  )
}
