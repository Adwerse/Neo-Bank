import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { Link } from 'react-router'
import { Card } from '../../../shared/ui/Card'
import { Input } from '../../../shared/ui/Input'
import { Button } from '../../../shared/ui/Button'
import { ErrorText } from '../../../shared/ui/ErrorText'
import { isApiError } from '../../../shared/api-client/ApiError'
import { forgotPassword } from '../api'
import { forgotPasswordSchema, type ForgotPasswordFormValues } from '../schemas'
import styles from './ForgotPasswordPage.module.css'

// Matches the backend's own uniform response — it deliberately replies the
// same way whether or not the email is registered, to avoid leaking account
// existence. Showing anything more specific here would undo that.
const NEUTRAL_MESSAGE = 'Если такой email зарегистрирован, на него отправлен код для сброса пароля.'

export function ForgotPasswordPage() {
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
      if (isApiError(err) && err.status === 429) {
        setServerError('Слишком много запросов. Подождите немного и попробуйте снова.')
      } else {
        setServerError('Не удалось отправить запрос, попробуйте ещё раз')
      }
    }
  }

  if (submitted) {
    return (
      <Card>
        <h1>Восстановление пароля</h1>
        <p>{NEUTRAL_MESSAGE}</p>
        <p className={styles.footerLink}>
          <Link to="/reset-password">Ввести код</Link>
        </p>
      </Card>
    )
  }

  return (
    <Card>
      <h1>Восстановление пароля</h1>
      <form className={styles.form} onSubmit={handleSubmit(onSubmit)} noValidate>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="email">
            Email
          </label>
          <Input id="email" type="email" autoComplete="email" {...register('email')} />
          {errors.email && <ErrorText>{errors.email.message}</ErrorText>}
        </div>
        {serverError && <ErrorText>{serverError}</ErrorText>}
        <Button type="submit" disabled={isSubmitting}>
          {isSubmitting ? 'Отправка...' : 'Отправить код'}
        </Button>
      </form>
      <p className={styles.footerLink}>
        <Link to="/login">Назад ко входу</Link>
      </p>
    </Card>
  )
}
