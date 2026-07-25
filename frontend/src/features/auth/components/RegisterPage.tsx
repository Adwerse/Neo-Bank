import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { useNavigate } from 'react-router'
import { Card } from '../../../shared/ui/Card'
import { Input } from '../../../shared/ui/Input'
import { Button } from '../../../shared/ui/Button'
import { ErrorText } from '../../../shared/ui/ErrorText'
import { isApiError } from '../../../shared/api-client/ApiError'
import { register as registerUser } from '../api'
import { registerSchema, type RegisterFormValues } from '../schemas'
import styles from './RegisterPage.module.css'

export function RegisterPage() {
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
      if (isApiError(err)) {
        setServerError(err.status === 409 ? 'Этот email уже зарегистрирован' : err.message)
      } else {
        setServerError('Не удалось выполнить регистрацию, попробуйте ещё раз')
      }
    }
  }

  return (
    <Card>
      <h1>Регистрация</h1>
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
          <Input id="password" type="password" autoComplete="new-password" {...register('password')} />
          {errors.password && <ErrorText>{errors.password.message}</ErrorText>}
        </div>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="confirmPassword">
            Подтверждение пароля
          </label>
          <Input
            id="confirmPassword"
            type="password"
            autoComplete="new-password"
            {...register('confirmPassword')}
          />
          {errors.confirmPassword && <ErrorText>{errors.confirmPassword.message}</ErrorText>}
        </div>
        {serverError && <ErrorText>{serverError}</ErrorText>}
        <Button type="submit" disabled={isSubmitting}>
          {isSubmitting ? 'Регистрация...' : 'Зарегистрироваться'}
        </Button>
      </form>
    </Card>
  )
}
