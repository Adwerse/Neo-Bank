import { useEffect, useState } from 'react'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { Navigate, useLocation, useNavigate } from 'react-router'
import { Card } from '../../../shared/ui/Card'
import { Input } from '../../../shared/ui/Input'
import { Button } from '../../../shared/ui/Button'
import { ErrorText } from '../../../shared/ui/ErrorText'
import { isApiError } from '../../../shared/api-client/ApiError'
import { errorMessage } from '../../../shared/errorMessages'
import { pluralize } from '../../../shared/locale'
import { useDocumentTitle } from '../../../shared/ui/useDocumentTitle'
import { resendVerification, verifyEmail } from '../api'
import { verifyCodeSchema, type VerifyCodeFormValues } from '../schemas'
import type { paths } from '../../../shared/api-client/schema'
import styles from './VerifyEmailPage.module.css'

type VerifyEmailErrorBody = paths['/auth/verify-email']['post']['responses']['400']['content']['application/json']

const RESEND_COOLDOWN_SECONDS = 60

// The backend returns one of these stable codes for every verify-email
// failure (see gateway/openapi.yaml's Error schema) — distinguishing
// cases means matching on these, not the human text (that's this
// component's own job now).
const WRONG_CODE = 'invalid_verification_code'
const CODE_EXPIRED = 'verification_code_expired'
const TOO_MANY_ATTEMPTS = 'too_many_verification_attempts'
const NO_ACTIVE_CODE = 'no_active_verification_code'

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
  useDocumentTitle('Verify email')
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
      navigate('/login', { state: { message: 'Email verified, please log in' } })
    } catch (err) {
      if (!isApiError(err)) {
        setVerifyError('Could not verify the code, please try again')
        return
      }
      const body = err.body as VerifyEmailErrorBody | undefined
      switch (err.message) {
        case WRONG_CODE: {
          const remaining = body?.attempts_remaining
          setVerifyError(
            typeof remaining === 'number'
              ? `Incorrect code — ${remaining} ${pluralize(remaining, 'attempt', 'attempts')} remaining`
              : 'Incorrect code, please try again',
          )
          break
        }
        case CODE_EXPIRED:
        case TOO_MANY_ATTEMPTS:
        case NO_ACTIVE_CODE:
          setVerifyError(errorMessage(err.message))
          setMustResend(true)
          break
        default:
          setVerifyError(errorMessage(err.message))
      }
    }
  }

  async function onResend() {
    setResendError(null)
    setResendNotice(null)
    setIsResending(true)
    try {
      await resendVerification({ email })
      setResendNotice('A new code has been sent to your email')
      setMustResend(false)
      setVerifyError(null)
    } catch (err) {
      // A 429 here means a code is already on its way (client/server
      // cooldowns briefly disagreeing) — not worth alarming the user over,
      // the button just stays locked for the rest of the countdown.
      if (isApiError(err) && err.status !== 429) {
        setResendError(errorMessage(err.message))
      }
    } finally {
      setIsResending(false)
      setSecondsLeft(RESEND_COOLDOWN_SECONDS)
    }
  }

  return (
    <Card>
      <h1>Verify email</h1>
      <p className={styles.hint}>We sent a 6-digit code to {email}</p>
      <form className={styles.form} onSubmit={handleSubmit(onSubmit)} noValidate>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="code">
            Verification code
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
          Verify
        </Button>
      </form>
      <div className={styles.resend}>
        <Button type="button" variant="secondary" loading={isResending} disabled={secondsLeft > 0} onClick={onResend}>
          {secondsLeft > 0 ? `Resend code (${secondsLeft})` : 'Resend code'}
        </Button>
        {resendNotice && <p className={styles.notice}>{resendNotice}</p>}
        {resendError && <ErrorText>{resendError}</ErrorText>}
      </div>
    </Card>
  )
}
