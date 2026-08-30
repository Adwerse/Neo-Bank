import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { useNavigate } from 'react-router'
import { Card } from '../../../shared/ui/Card'
import { Input } from '../../../shared/ui/Input'
import { Button } from '../../../shared/ui/Button'
import { ErrorText } from '../../../shared/ui/ErrorText'
import { isApiError } from '../../../shared/api-client/ApiError'
import { errorMessage } from '../../../shared/errorMessages'
import { useDocumentTitle } from '../../../shared/ui/useDocumentTitle'
import { register as registerUser } from '../api'
import { registerSchema, type RegisterFormValues } from '../schemas'
import styles from './RegisterPage.module.css'

export function RegisterPage() {
  useDocumentTitle('Register')
  const navigate = useNavigate()
  const [serverError, setServerError] = useState<string | null>(null)
  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<RegisterFormValues>({ resolver: zodResolver(registerSchema) })

  async function onSubmit(values: RegisterFormValues) {
    setServerError(null)
    try {
      await registerUser({ email: values.email, password: values.password })
      navigate('/verify-email', { state: { email: values.email } })
    } catch (err) {
      // Client-side zod validation is UX, not security — the backend
      // validates independently (duplicate email, weak password, ...) and
      // those errors need to reach the user too, not just get swallowed.
      setServerError(isApiError(err) ? errorMessage(err.message) : 'Could not register, please try again')
    }
  }

  return (
    <Card>
      <h1>Register</h1>
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
          <label className={styles.label} htmlFor="password">
            Password
          </label>
          <Input
            id="password"
            type="password"
            autoComplete="new-password"
            error={Boolean(errors.password)}
            aria-describedby={errors.password ? 'password-error' : undefined}
            {...register('password')}
          />
          {errors.password && <ErrorText id="password-error">{errors.password.message}</ErrorText>}
        </div>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="confirmPassword">
            Confirm password
          </label>
          <Input
            id="confirmPassword"
            type="password"
            autoComplete="new-password"
            error={Boolean(errors.confirmPassword)}
            aria-describedby={errors.confirmPassword ? 'confirmPassword-error' : undefined}
            {...register('confirmPassword')}
          />
          {errors.confirmPassword && <ErrorText id="confirmPassword-error">{errors.confirmPassword.message}</ErrorText>}
        </div>
        {serverError && <ErrorText>{serverError}</ErrorText>}
        <Button type="submit" loading={isSubmitting}>
          Register
        </Button>
      </form>
    </Card>
  )
}
