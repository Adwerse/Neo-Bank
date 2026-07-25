import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { Link, useLocation, useNavigate } from 'react-router'
import { Card } from '../../../shared/ui/Card'
import { Input } from '../../../shared/ui/Input'
import { Button } from '../../../shared/ui/Button'
import { ErrorText } from '../../../shared/ui/ErrorText'
import { isApiError } from '../../../shared/api-client/ApiError'
import { useAuth } from '../AuthContext'
import { resendVerification } from '../api'
import { loginSchema, type LoginFormValues } from '../schemas'
import styles from './LoginPage.module.css'

// The backend deliberately collapses "no such email" and "wrong password"
// into the same message to resist account enumeration — the UI must not
// undo that by guessing which one it was.
const INVALID_CREDENTIALS = 'invalid credentials'
const EMAIL_NOT_VERIFIED = 'email not verified'

export function LoginPage() {
  const location = useLocation()
  const navigate = useNavigate()
  const { login } = useAuth()
  const infoMessage = (location.state as { message?: string } | null)?.message

  const [serverError, setServerError] = useState<string | null>(null)
  const [unverifiedEmail, setUnverifiedEmail] = useState<string | null>(null)

  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<LoginFormValues>({ resolver: zodResolver(loginSchema) })

  async function onSubmit(values: LoginFormValues) {
    setServerError(null)
    setUnverifiedEmail(null)
    try {
      await login(values.email, values.password)
      navigate('/dashboard', { replace: true })
    } catch (err) {
      if (!isApiError(err)) {
        setServerError('Не удалось войти, попробуйте ещё раз')
        return
      }
      if (err.message === INVALID_CREDENTIALS) {
        setServerError('Неверный email или пароль')
      } else if (err.message === EMAIL_NOT_VERIFIED) {
        setUnverifiedEmail(values.email)
      } else {
        setServerError(err.message)
      }
    }
  }

  async function onSendVerificationCode() {
    if (!unverifiedEmail) return
    const email = unverifiedEmail
    // VerifyEmailPage assumes a code was just sent (its resend countdown
    // starts at 60 immediately) — true here only because this click just
    // triggered one. Any error (incl. a 429 from a very recent code) is
    // fine to ignore: the code-entry screen's own resend handles it.
    await resendVerification({ email }).catch(() => {})
    navigate('/verify-email', { state: { email } })
  }

  return (
    <Card>
      <h1>Вход</h1>
      {infoMessage && <p className={styles.notice}>{infoMessage}</p>}
      <form className={styles.form} onSubmit={handleSubmit(onSubmit)} noValidate>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="email">
            Email
          </label>
          <Input id="email" type="email" autoComplete="email" {...register('email')} />
          {errors.email && <ErrorText>{errors.email.message}</ErrorText>}
        </div>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="password">
            Пароль
          </label>
          <Input id="password" type="password" autoComplete="current-password" {...register('password')} />
          {errors.password && <ErrorText>{errors.password.message}</ErrorText>}
        </div>
        {serverError && <ErrorText>{serverError}</ErrorText>}
        {unverifiedEmail && (
          <ErrorText>
            Email не подтверждён.{' '}
            <button type="button" className={styles.linkButton} onClick={onSendVerificationCode}>
              Отправить код подтверждения
            </button>
          </ErrorText>
        )}
        <Button type="submit" disabled={isSubmitting}>
          {isSubmitting ? 'Вход...' : 'Войти'}
        </Button>
      </form>
      <p className={styles.footerLink}>
        <Link to="/forgot-password">Забыли пароль?</Link>
      </p>
    </Card>
  )
}
