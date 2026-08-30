import { useState } from 'react'
import { useNavigate } from 'react-router'
import { Badge } from '../../../shared/ui/Badge'
import { Banner } from '../../../shared/ui/Banner'
import { Button } from '../../../shared/ui/Button'
import { Card } from '../../../shared/ui/Card'
import { CopyButton } from '../../../shared/ui/CopyButton'
import { Modal } from '../../../shared/ui/Modal'
import { Skeleton } from '../../../shared/ui/Skeleton'
import { useDocumentTitle } from '../../../shared/ui/useDocumentTitle'
import { LogoutIcon } from '../../../shared/ui/icons'
import { getAccessTokenEmail } from '../../../shared/api-client/jwt'
import { useAuth } from '../../auth/AuthContext'
import { formatIban } from '../../transfers/iban'
import { ACCOUNT_STATUS_LABELS } from '../../accounts/statusLabels'
import { useMe } from '../../accounts/useMe'
import { useProfile } from '../useProfile'
import { AvatarUploader } from './AvatarUploader'
import { DisplayNameEditor } from './DisplayNameEditor'
import styles from './ProfilePage.module.css'

// Same success/warning/danger convention as operationLabels.ts's status
// badges — account status just isn't one of the entry types that helper
// covers, so its own tiny map lives here instead.
const ACCOUNT_STATUS_VARIANT: Record<string, 'success' | 'warning' | 'danger'> = {
  active: 'success',
  frozen: 'warning',
  closed: 'danger',
}

export function ProfilePage() {
  useDocumentTitle('Profile')
  const { data: profile, isLoading: profileLoading, isError: profileError, refetch: refetchProfile } = useProfile()
  const { data: account, isLoading: accountLoading, isError: accountError, refetch: refetchAccount } = useMe()
  const { logout } = useAuth()
  const navigate = useNavigate()
  const [confirmOpen, setConfirmOpen] = useState(false)
  const [loggingOut, setLoggingOut] = useState(false)

  async function handleLogout() {
    setLoggingOut(true)
    try {
      await logout()
      navigate('/login', { replace: true })
    } finally {
      // Only reached if logout() itself threw before clearing the
      // session — on the ordinary path this component has already
      // unmounted (navigate away) by the time this would run.
      setLoggingOut(false)
    }
  }

  if (profileLoading) {
    return (
      <div className={styles.page}>
        <Card>
          <div className={styles.loadingHeader}>
            <Skeleton className={styles.avatarSkeleton} />
            <Skeleton className={styles.nameSkeleton} />
          </div>
        </Card>
      </div>
    )
  }

  if (profileError || !profile) {
    return (
      <div className={styles.page}>
        <Card>
          <Banner variant="warning">Could not load your profile.</Banner>
          <Button className={styles.retryButton} onClick={() => refetchProfile()}>
            Retry
          </Button>
        </Card>
      </div>
    )
  }

  return (
    <div className={styles.page}>
      <Card>
        <AvatarUploader profile={profile} />
        <div className={styles.nameSection}>
          <DisplayNameEditor profile={profile} />
          <span className={styles.email}>{getAccessTokenEmail()}</span>
        </div>
      </Card>

      <Card>
        <h2>Account details</h2>
        {accountLoading && (
          <div className={styles.detailsSkeleton}>
            <Skeleton className={styles.detailSkeletonRow} />
            <Skeleton className={styles.detailSkeletonRow} />
          </div>
        )}
        {accountError && (
          <div className={styles.accountError}>
            <Banner variant="warning">Account details are temporarily unavailable.</Banner>
            <Button variant="secondary" onClick={() => refetchAccount()}>
              Retry
            </Button>
          </div>
        )}
        {account && (
          <div className={styles.details}>
            <div className={styles.detailRow}>
              <span className={styles.detailLabel}>IBAN</span>
              <span className={styles.detailValue}>{formatIban(account.iban)}</span>
              <CopyButton value={account.iban} label="Copy IBAN" />
            </div>
            <div className={styles.detailRow}>
              <span className={styles.detailLabel}>Account number</span>
              <span className={styles.detailValue}>{account.account_number}</span>
              <CopyButton value={account.account_number} label="Copy account number" />
            </div>
            <div className={styles.detailRow}>
              <span className={styles.detailLabel}>Status</span>
              <Badge variant={ACCOUNT_STATUS_VARIANT[account.status] ?? 'pending'}>
                {ACCOUNT_STATUS_LABELS[account.status] ?? account.status}
              </Badge>
            </div>
          </div>
        )}
      </Card>

      <Card>
        <Button variant="secondary" className={styles.logoutButton} onClick={() => setConfirmOpen(true)}>
          <LogoutIcon size={15} />
          Log out
        </Button>
      </Card>

      <Modal isOpen={confirmOpen} onClose={() => setConfirmOpen(false)} title="Log out?">
        <p className={styles.confirmText}>You can log back in at any time with your email and password.</p>
        <div className={styles.confirmActions}>
          <Button onClick={handleLogout} loading={loggingOut}>
            Log out
          </Button>
          <Button variant="secondary" disabled={loggingOut} onClick={() => setConfirmOpen(false)}>
            Cancel
          </Button>
        </div>
      </Modal>
    </div>
  )
}
