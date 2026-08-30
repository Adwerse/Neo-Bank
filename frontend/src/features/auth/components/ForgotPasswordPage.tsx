import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { Link } from 'react-router'
import { Card } from '../../../shared/ui/Card'
import { Input } from '../../../shared/ui/Input'
import { Button } from '../../../shared/ui/Button'
import { ErrorText } from '../../../shared/ui/ErrorText'
import { isApiError } from '../../../shared/api-client/ApiError'
import { errorMessage } from '../../../shared/errorMessages'
import { useDocumentTitle } from '../../../shared/ui/useDocumentTitle'
import { forgotPassword } from '../api'
import { forgotPasswordSchema, type ForgotPasswordFormValues } from '../schemas'
import styles from './ForgotPasswordPage.module.css'

// Matches the backend's own uniform response — it deliberately replies the
// same way whether or not the email is registered, to avoid leaking account
// existence. Showing anything more specific here would undo that.
const NEUTRAL_MESSAGE = 'If that email is registered, a password reset code has been sent to it.'

export function ForgotPasswordPage() {
  useDocumentTitle('Reset password')
  const [submitted, setSubmitted] = useState(false)
  const [serverError, setServerError] = useState<string | null>(null)

  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<ForgotPasswordFormValues>({ resolver: zodResolver(forgotPasswordSchema) })

  async function onSubmit(values: ForgotPasswordFormValues) {
    setServerError(null)
    try {
      await forgotPassword({ email: values.email })
      setSubmitted(true)
    } catch (err) {
      setServerError(isApiError(err) ? errorMessage(err.message) : 'Could not send the request, please try again')
    }
  }

  if (submitted) {
    return (
      <Card>
        <h1>Reset password</h1>
        <p>{NEUTRAL_MESSAGE}</p>
        <p className={styles.footerLink}>
          <Link to="/reset-password">Enter code</Link>
        </p>
      </Card>
    )
  }

  return (
    <Card>
      <h1>Reset password</h1>
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
        {serverError && <ErrorText>{serverError}</ErrorText>}
        <Button type="submit" loading={isSubmitting}>
          Send code
        </Button>
      </form>
      <p className={styles.footerLink}>
        <Link to="/login">Back to login</Link>
      </p>
    </Card>
  )
}
