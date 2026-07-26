import { useRef, useState } from 'react'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { useQueryClient } from '@tanstack/react-query'
import { Card } from '../../../shared/ui/Card'
import { Input } from '../../../shared/ui/Input'
import { Button } from '../../../shared/ui/Button'
import { ErrorText } from '../../../shared/ui/ErrorText'
import { Banner } from '../../../shared/ui/Banner'
import { isApiError } from '../../../shared/api-client/ApiError'
import { formatMoney } from '../../accounts/money'
import { createTransfer, type TransferResult } from '../api'
import { parseAmountToCents } from '../money'
import { transferSchema, type TransferFormValues } from '../schemas'
import styles from './TransferForm.module.css'

const FAILURE_REASON_LABELS: Record<string, string> = {
  insufficient_funds: 'Недостаточно средств',
  account_not_found: 'Счёт не найден',
  invalid_amount: 'Некорректная сумма',
  ledger_internal_error: 'Временная ошибка перевода, попробуйте позже',
}

function failureReasonMessage(reason?: string): string {
  if (!reason) return 'Перевод не выполнен'
  return FAILURE_REASON_LABELS[reason] ?? reason
}

// Matches raw backend error strings (services/transfers-svc/http.go /
// transfer.go) to Russian messages, same lookup-with-fallback pattern as
// LoginPage's INVALID_CREDENTIALS/EMAIL_NOT_VERIFIED.
const API_ERROR_LABELS: Record<string, string> = {
  'recipient not found': 'Получатель с таким номером счёта не найден',
  'cannot transfer to your own account': 'Нельзя перевести самому себе',
  'recipient account is closed': 'Счёт получателя закрыт',
  'sender account is not active': 'Ваш счёт временно не может отправлять переводы',
  'idempotency key already used with different parameters': 'Что-то пошло не так, попробуйте создать перевод заново',
  'invalid amount': 'Введите сумму больше нуля',
  'account not found': 'Ваш счёт ещё создаётся — попробуйте через несколько секунд',
}

function apiErrorMessage(message: string): string {
  return API_ERROR_LABELS[message] ?? message
}

export function TransferForm() {
  const queryClient = useQueryClient()

  // Generated once when the form mounts ("form open"), not inside the
  // submit handler — a retry of the same in-flight attempt (double-click,
  // resubmitting after a dropped response) must reuse this exact value so
  // transfers-svc's idempotency check can recognize it as the same
  // request. It's only replaced by handleNewTransfer, once the user has
  // seen a result and deliberately starts a different transfer.
  const idempotencyKeyRef = useRef(crypto.randomUUID())

  const [result, setResult] = useState<TransferResult | null>(null)
  const [serverError, setServerError] = useState<string | null>(null)

  const {
    register,
    handleSubmit,
    reset,
    formState: { errors, isSubmitting },
  } = useForm<TransferFormValues>({ resolver: zodResolver(transferSchema) })

  async function onSubmit(values: TransferFormValues) {
    setServerError(null)
    const amountCents = parseAmountToCents(values.amount)
    if (amountCents === null) {
      setServerError('Введите корректную сумму')
      return
    }
    try {
      const response = await createTransfer(
        { recipient_account_number: values.recipientAccountNumber, amount: amountCents },
        idempotencyKeyRef.current,
      )
      setResult(response)
      // A Transfer object came back — a row now exists (or already did),
      // so both the balance and the history may have changed.
      queryClient.invalidateQueries({ queryKey: ['accounts', 'me'] })
      queryClient.invalidateQueries({ queryKey: ['transfers', 'history'] })
    } catch (err) {
      setResult(null)
      setServerError(isApiError(err) ? apiErrorMessage(err.message) : 'Не удалось выполнить перевод, попробуйте ещё раз')
    }
  }

  function handleNewTransfer() {
    idempotencyKeyRef.current = crypto.randomUUID()
    setResult(null)
    setServerError(null)
    reset()
  }

  return (
    <Card>
      <h2>Перевод</h2>
      <form className={styles.form} onSubmit={handleSubmit(onSubmit)} noValidate>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="recipientAccountNumber">
            Номер счёта получателя
          </label>
          <Input id="recipientAccountNumber" autoComplete="off" {...register('recipientAccountNumber')} />
          {errors.recipientAccountNumber && <ErrorText>{errors.recipientAccountNumber.message}</ErrorText>}
        </div>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="amount">
            Сумма
          </label>
          <Input id="amount" inputMode="decimal" placeholder="100.00" {...register('amount')} />
          {errors.amount && <ErrorText>{errors.amount.message}</ErrorText>}
        </div>
        {serverError && <ErrorText>{serverError}</ErrorText>}
        <Button type="submit" disabled={isSubmitting}>
          {isSubmitting ? 'Отправка...' : 'Отправить'}
        </Button>
      </form>

      {result && (
        <div className={styles.result}>
          {result.status === 'completed' && (
            <Banner variant="success">Перевод выполнен: {formatMoney(result.amount, 'EUR')}.</Banner>
          )}
          {result.status === 'failed' && <Banner variant="danger">{failureReasonMessage(result.failure_reason)}</Banner>}
          {result.status === 'pending' && result.message && (
            <Banner variant="warning">
              Перевод обрабатывается, статус неизвестен — проверьте историю переводов ниже.
            </Banner>
          )}
          <Button variant="secondary" type="button" className={styles.newTransferButton} onClick={handleNewTransfer}>
            Новый перевод
          </Button>
        </div>
      )}
    </Card>
  )
}
