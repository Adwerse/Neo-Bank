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
import { useMe } from '../../accounts/useMe'
import { useProfile } from '../useProfile'
import { AvatarUploader } from './AvatarUploader'
import { DisplayNameEditor } from './DisplayNameEditor'
import styles from './ProfilePage.module.css'

const STATUS_LABELS: Record<string, string> = {
  active: 'Активен',
  frozen: 'Заморожен',
  closed: 'Закрыт',
}

const STATUS_VARIANT: Record<string, 'success' | 'warning' | 'danger'> = {
  active: 'success',
  frozen: 'warning',
  closed: 'danger',
}

export function ProfilePage() {
  useDocumentTitle('Профиль')
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
          <Banner variant="warning">Не удалось загрузить профиль.</Banner>
          <Button className={styles.retryButton} onClick={() => refetchProfile()}>
            Повторить
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
        <h2>Реквизиты счёта</h2>
        {accountLoading && (
          <div className={styles.detailsSkeleton}>
            <Skeleton className={styles.detailSkeletonRow} />
            <Skeleton className={styles.detailSkeletonRow} />
          </div>
        )}
        {accountError && (
          <div className={styles.accountError}>
            <Banner variant="warning">Реквизиты счёта временно недоступны.</Banner>
            <Button variant="secondary" onClick={() => refetchAccount()}>
              Повторить
            </Button>
          </div>
        )}
        {account && (
          <div className={styles.details}>
            <div className={styles.detailRow}>
              <span className={styles.detailLabel}>IBAN</span>
              <span className={styles.detailValue}>{formatIban(account.iban)}</span>
              <CopyButton value={account.iban} label="Скопировать IBAN" />
            </div>
            <div className={styles.detailRow}>
              <span className={styles.detailLabel}>Номер счёта</span>
              <span className={styles.detailValue}>{account.account_number}</span>
              <CopyButton value={account.account_number} label="Скопировать номер счёта" />
            </div>
            <div className={styles.detailRow}>
              <span className={styles.detailLabel}>Статус</span>
              <Badge variant={STATUS_VARIANT[account.status] ?? 'pending'}>
                {STATUS_LABELS[account.status] ?? account.status}
              </Badge>
            </div>
          </div>
        )}
      </Card>

      <Card>
        <Button variant="secondary" className={styles.logoutButton} onClick={() => setConfirmOpen(true)}>
          <LogoutIcon size={15} />
          Выйти из аккаунта
        </Button>
      </Card>

      <Modal isOpen={confirmOpen} onClose={() => setConfirmOpen(false)} title="Выйти из аккаунта?">
        <p className={styles.confirmText}>Вы сможете войти снова в любой момент — по email и паролю.</p>
        <div className={styles.confirmActions}>
          <Button onClick={handleLogout} loading={loggingOut}>
            Выйти
          </Button>
          <Button variant="secondary" disabled={loggingOut} onClick={() => setConfirmOpen(false)}>
            Отмена
          </Button>
        </div>
      </Modal>
    </div>
  )
}
