import { useEffect, useState } from 'react'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { Navigate, useLocation, useNavigate } from 'react-router'
import { Card } from '../../../shared/ui/Card'
import { Input } from '../../../shared/ui/Input'
import { Button } from '../../../shared/ui/Button'
import { ErrorText } from '../../../shared/ui/ErrorText'
import { isApiError } from '../../../shared/api-client/ApiError'
import { useDocumentTitle } from '../../../shared/ui/useDocumentTitle'
import { resendVerification, verifyEmail } from '../api'
import { verifyCodeSchema, type VerifyCodeFormValues } from '../schemas'
import type { paths } from '../../../shared/api-client/schema'
import styles from './VerifyEmailPage.module.css'

type VerifyEmailErrorBody = paths['/auth/verify-email']['post']['responses']['400']['content']['application/json']

const RESEND_COOLDOWN_SECONDS = 60

// The backend collapses every verify-email failure into the same
// {error: string} shape (no error-code field), so distinguishing cases in
// the UI means matching on these exact strings from services/auth-svc/verify.go.
const WRONG_CODE = 'invalid verification code'
const CODE_EXPIRED = 'verification code has expired'
const TOO_MANY_ATTEMPTS = 'too many failed attempts, request a new code'
const NO_ACTIVE_CODE = 'no active verification code, request a new one'

export function VerifyEmailPage() {
  const location = useLocation()
  const email = (location.state as { email?: string } | null)?.email

  // Email only ever arrives via router state (never the URL) — a direct
  // hit or refresh on this route has nothing to verify.
  if (!email) {
    return <Navigate to="/register" replace />
  }

  return <VerifyEmailForm email={email} />
}

function VerifyEmailForm({ email }: { email: string }) {
  useDocumentTitle('Подтверждение email')
  const navigate = useNavigate()

  const [verifyError, setVerifyError] = useState<string | null>(null)
  const [mustResend, setMustResend] = useState(false)
  const [resendError, setResendError] = useState<string | null>(null)
  const [resendNotice, setResendNotice] = useState<string | null>(null)
  const [secondsLeft, setSecondsLeft] = useState(RESEND_COOLDOWN_SECONDS)
  const [isResending, setIsResending] = useState(false)

  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<VerifyCodeFormValues>({ resolver: zodResolver(verifyCodeSchema) })

  useEffect(() => {
    if (secondsLeft <= 0) return
    const timer = setTimeout(() => setSecondsLeft((s) => s - 1), 1000)
    return () => clearTimeout(timer)
  }, [secondsLeft])

  async function onSubmit(values: VerifyCodeFormValues) {
    setVerifyError(null)
    try {
      await verifyEmail({ email, code: values.code })
      navigate('/login', { state: { message: 'Email подтверждён, войдите' } })
    } catch (err) {
      if (!isApiError(err)) {
        setVerifyError('Не удалось подтвердить код, попробуйте ещё раз')
        return
      }
      const body = err.body as VerifyEmailErrorBody | undefined
      switch (err.message) {
        case WRONG_CODE: {
          const remaining = body?.attempts_remaining
          setVerifyError(
            typeof remaining === 'number'
              ? `Неверный код, осталось попыток: ${remaining}`
              : 'Неверный код, попробуйте ещё раз',
          )
          break
        }
        case CODE_EXPIRED:
          setVerifyError('Код истёк. Запросите новый код.')
          setMustResend(true)
          break
        case TOO_MANY_ATTEMPTS:
          setVerifyError('Попытки исчерпаны. Запросите новый код.')
          setMustResend(true)
          break
        case NO_ACTIVE_CODE:
          setVerifyError('Активный код не найден. Запросите новый код.')
          setMustResend(true)
          break
        default:
          setVerifyError(err.message)
      }
    }
  }

  async function onResend() {
    setResendError(null)
    setResendNotice(null)
    setIsResending(true)
    try {
      await resendVerification({ email })
      setResendNotice('Новый код отправлен на почту')
      setMustResend(false)
      setVerifyError(null)
    } catch (err) {
      // A 429 here means a code is already on its way (client/server
      // cooldowns briefly disagreeing) — not worth alarming the user over,
      // the button just stays locked for the rest of the countdown.
      if (isApiError(err) && err.status !== 429) {
        setResendError(err.message)
      }
    } finally {
      setIsResending(false)
      setSecondsLeft(RESEND_COOLDOWN_SECONDS)
    }
  }

  return (
    <Card>
      <h1>Подтверждение email</h1>
      <p className={styles.hint}>Мы отправили 6-значный код на {email}</p>
      <form className={styles.form} onSubmit={handleSubmit(onSubmit)} noValidate>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="code">
            Код подтверждения
          </label>
          <Input
            id="code"
            inputMode="numeric"
            autoComplete="one-time-code"
            maxLength={6}
            autoFocus
            disabled={mustResend}
            error={Boolean(errors.code)}
            aria-describedby={errors.code ? 'code-error' : undefined}
            {...register('code')}
          />
          {errors.code && <ErrorText id="code-error">{errors.code.message}</ErrorText>}
        </div>
        {verifyError && <ErrorText>{verifyError}</ErrorText>}
        <Button type="submit" loading={isSubmitting} disabled={mustResend}>
          Подтвердить
        </Button>
      </form>
      <div className={styles.resend}>
        <Button type="button" variant="secondary" loading={isResending} disabled={secondsLeft > 0} onClick={onResend}>
          {secondsLeft > 0 ? `Отправить код заново (${secondsLeft})` : 'Отправить код заново'}
        </Button>
        {resendNotice && <p className={styles.notice}>{resendNotice}</p>}
        {resendError && <ErrorText>{resendError}</ErrorText>}
      </div>
    </Card>
  )
}
