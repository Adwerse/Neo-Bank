import { useDocumentTitle } from '../../../shared/ui/useDocumentTitle'
import { DepositForm } from './DepositForm'
import styles from './DepositPage.module.css'

export function DepositPage() {
  useDocumentTitle('Deposit')
  return (
    <div className={styles.page}>
      <DepositForm />
    </div>
  )
}
