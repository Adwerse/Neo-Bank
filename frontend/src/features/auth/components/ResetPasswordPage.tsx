import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { Link, useNavigate } from 'react-router'
import { Card } from '../../../shared/ui/Card'
import { Input } from '../../../shared/ui/Input'
import { Button } from '../../../shared/ui/Button'
import { ErrorText } from '../../../shared/ui/ErrorText'
import { isApiError } from '../../../shared/api-client/ApiError'
import { errorMessage } from '../../../shared/errorMessages'
import { useDocumentTitle } from '../../../shared/ui/useDocumentTitle'
import { resetPassword } from '../api'
import { useAuth } from '../AuthContext'
import { resetPasswordSchema, type ResetPasswordFormValues } from '../schemas'
import styles from './ResetPasswordPage.module.css'

export function ResetPasswordPage() {
  useDocumentTitle('New password')
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
      navigate('/login', { state: { message: 'Password changed, please log in with your new password' } })
    } catch (err) {
      setServerError(isApiError(err) ? errorMessage(err.message) : 'Could not reset your password, please try again')
    }
  }

  return (
    <Card>
      <h1>New password</h1>
      <form className={styles.form} onSubmit={handleSubmit(onSubmit)} noValidate>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="email">
            Email
          </label>
          <Input
            id="email"
            type="email"
            autoComplete="email"
            autoFocus
            error={Boolean(errors.email)}
            aria-describedby={errors.email ? 'email-error' : undefined}
            {...register('email')}
          />
          {errors.email && <ErrorText id="email-error">{errors.email.message}</ErrorText>}
        </div>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="code">
            Code from the email
          </label>
          <Input
            id="code"
            inputMode="numeric"
            autoComplete="one-time-code"
            maxLength={6}
            error={Boolean(errors.code)}
            aria-describedby={errors.code ? 'code-error' : undefined}
            {...register('code')}
          />
          {errors.code && <ErrorText id="code-error">{errors.code.message}</ErrorText>}
        </div>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="newPassword">
            New password
          </label>
          <Input
            id="newPassword"
            type="password"
            autoComplete="new-password"
            error={Boolean(errors.newPassword)}
            aria-describedby={errors.newPassword ? 'newPassword-error' : undefined}
            {...register('newPassword')}
          />
          {errors.newPassword && <ErrorText id="newPassword-error">{errors.newPassword.message}</ErrorText>}
        </div>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="confirmNewPassword">
            Confirm password
          </label>
          <Input
            id="confirmNewPassword"
            type="password"
            autoComplete="new-password"
            error={Boolean(errors.confirmNewPassword)}
            aria-describedby={errors.confirmNewPassword ? 'confirmNewPassword-error' : undefined}
            {...register('confirmNewPassword')}
          />
          {errors.confirmNewPassword && (
            <ErrorText id="confirmNewPassword-error">{errors.confirmNewPassword.message}</ErrorText>
          )}
        </div>
        {serverError && <ErrorText>{serverError}</ErrorText>}
        <Button type="submit" loading={isSubmitting}>
          Save new password
        </Button>
      </form>
      <p className={styles.footerLink}>
        <Link to="/forgot-password">Request a new code</Link>
      </p>
    </Card>
  )
}
