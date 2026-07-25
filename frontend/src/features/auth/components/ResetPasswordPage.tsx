import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { Link, useNavigate } from 'react-router'
import { Card } from '../../../shared/ui/Card'
import { Input } from '../../../shared/ui/Input'
import { Button } from '../../../shared/ui/Button'
import { ErrorText } from '../../../shared/ui/ErrorText'
import { isApiError } from '../../../shared/api-client/ApiError'
import { resetPassword } from '../api'
import { useAuth } from '../AuthContext'
import { resetPasswordSchema, type ResetPasswordFormValues } from '../schemas'
import styles from './ResetPasswordPage.module.css'

// Same {error: string} shape as /auth/verify-email, no error-code field —
// distinguishing cases means matching on these exact strings from
// services/auth-svc/reset.go.
const INVALID_OR_EXPIRED = 'invalid or expired reset code'
const CODE_EXPIRED = 'verification code has expired'
const TOO_MANY_ATTEMPTS = 'too many failed attempts, request a new code'
const WRONG_CODE = 'invalid verification code'

export function ResetPasswordPage() {
  const navigate = useNavigate()
  const { clearSession } = useAuth()
  const [serverError, setServerError] = useState<string | null>(null)

  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<ResetPasswordFormValues>({ resolver: zodResolver(resetPasswordSchema) })

  async function onSubmit(values: ResetPasswordFormValues) {
    setServerError(null)
    try {
      await resetPassword({ email: values.email, code: values.code, new_password: values.newPassword })
      // The backend just revoked every refresh token for this user — any
      // session stored locally in this browser is already dead, so clear it
      // here rather than letting a stale refresh token fail confusingly later.
      clearSession()
      navigate('/login', { state: { message: 'Пароль изменён, войдите с новым паролем' } })
    } catch (err) {
      if (!isApiError(err)) {
        setServerError('Не удалось сбросить пароль, попробуйте ещё раз')
        return
      }
      switch (err.message) {
        case INVALID_OR_EXPIRED:
          setServerError('Код недействителен или истёк. Запросите новый на странице восстановления пароля.')
          break
        case CODE_EXPIRED:
          setServerError('Код истёк. Запросите новый на странице восстановления пароля.')
          break
        case TOO_MANY_ATTEMPTS:
          setServerError('Попытки исчерпаны. Запросите новый код на странице восстановления пароля.')
          break
        case WRONG_CODE:
          setServerError('Неверный код.')
          break
        default:
          setServerError(err.message)
      }
    }
  }

  return (
    <Card>
      <h1>Новый пароль</h1>
      <form className={styles.form} onSubmit={handleSubmit(onSubmit)} noValidate>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="email">
            Email
          </label>
          <Input id="email" type="email" autoComplete="email" {...register('email')} />
          {errors.email && <ErrorText>{errors.email.message}</ErrorText>}
        </div>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="code">
            Код из письма
          </label>
          <Input id="code" inputMode="numeric" autoComplete="one-time-code" maxLength={6} {...register('code')} />
          {errors.code && <ErrorText>{errors.code.message}</ErrorText>}
        </div>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="newPassword">
            Новый пароль
          </label>
          <Input id="newPassword" type="password" autoComplete="new-password" {...register('newPassword')} />
          {errors.newPassword && <ErrorText>{errors.newPassword.message}</ErrorText>}
        </div>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="confirmNewPassword">
            Подтверждение пароля
          </label>
          <Input
            id="confirmNewPassword"
            type="password"
            autoComplete="new-password"
            {...register('confirmNewPassword')}
          />
          {errors.confirmNewPassword && <ErrorText>{errors.confirmNewPassword.message}</ErrorText>}
        </div>
        {serverError && <ErrorText>{serverError}</ErrorText>}
        <Button type="submit" disabled={isSubmitting}>
          {isSubmitting ? 'Сохранение...' : 'Сохранить новый пароль'}
        </Button>
      </form>
      <p className={styles.footerLink}>
        <Link to="/forgot-password">Запросить новый код</Link>
      </p>
    </Card>
  )
}
