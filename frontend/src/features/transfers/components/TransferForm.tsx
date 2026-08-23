import { useRef, useState } from 'react'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { useQueryClient } from '@tanstack/react-query'
import { Card } from '../../../shared/ui/Card'
import { Input } from '../../../shared/ui/Input'
import { Button } from '../../../shared/ui/Button'
import { ErrorText } from '../../../shared/ui/ErrorText'
import { Banner } from '../../../shared/ui/Banner'
import { Money } from '../../../shared/ui/Money'
import { isApiError } from '../../../shared/api-client/ApiError'
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
  'recipient not found': 'Получатель с таким IBAN не найден',
  'invalid IBAN': 'Проверьте IBAN получателя — неверный формат или контрольные цифры',
  'only transfers within this bank are supported': 'Переводы поддерживаются только внутри этого банка',
  'too many resolve attempts, try again later': 'Слишком много попыток, попробуйте позже',
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

// Deliberately separate from FAILURE_REASON_LABELS above: failure_reason
// holds two disjoint vocabularies depending on status (ledger codes when
// failed, fraud-svc's triggered rule name when rejected — see
// services/transfers-svc/transfer.go's Transfer.FailureReason doc comment).
// Unlike failureReasonMessage/apiErrorMessage, this deliberately does NOT
// fall back to the raw string: a raw rule name like "velocity_count" is an
// internal implementation detail, not something to show a user, and must
// never leak here.
const REJECTED_REASON_LABELS: Record<string, string> = {
  amount_threshold: 'превышен лимит суммы для одного перевода',
  velocity_count: 'слишком много переводов за короткое время',
  velocity_sum: 'превышена допустимая общая сумма переводов за короткое время',
}

function rejectedReasonClause(reason?: string): string {
  const clause = reason ? REJECTED_REASON_LABELS[reason] : undefined
  return clause ? `Причина: ${clause}.` : ''
}

// The only signal distinguishing this 202 cause from the pre-existing
// ledger-uncertain one: both share the identical TransferResult shape
// (status "pending" + optional message), so the message text itself is
// the discriminator. Matches services/transfers-svc/http.go's literal.
const FRAUD_CHECK_UNAVAILABLE_MESSAGE = 'fraud check unavailable, transfer still pending'

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
  // Tracks "the last known cause of a still-pending row was fraud-svc being
  // unavailable" independently of the current response's message: a retry
  // reuses the same Idempotency-Key, which (as long as the row is still
  // pending) hits transfers-svc's replay fast-path — that returns the bare
  // Transfer as-is, with no message at all (message is never persisted to
  // the transfers table, only generated fresh on a definite 202 response).
  // Without this flag, a second retry attempt would silently lose the
  // fraud-unavailable banner/button and fall back to "Новый перевод",
  // even though nothing was actually resolved.
  const [fraudUnavailable, setFraudUnavailable] = useState(false)

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
        { recipient_iban: values.recipientIban, amount: amountCents },
        idempotencyKeyRef.current,
      )
      setResult(response)
      if (response.status === 'pending' && response.message === FRAUD_CHECK_UNAVAILABLE_MESSAGE) {
        setFraudUnavailable(true)
      } else if (response.status !== 'pending') {
        setFraudUnavailable(false)
      }
      // else: status is "pending" with no message (the replay fast-path) —
      // deliberately leave fraudUnavailable exactly as it was, see the
      // comment on its declaration above.

      // A Transfer object came back — a row now exists (or already did),
      // so both the balance and the history may have changed.
      queryClient.invalidateQueries({ queryKey: ['accounts', 'me'] })
      queryClient.invalidateQueries({ queryKey: ['transfers', 'history'] })
    } catch (err) {
      setResult(null)
      setFraudUnavailable(false)
      setServerError(isApiError(err) ? apiErrorMessage(err.message) : 'Не удалось выполнить перевод, попробуйте ещё раз')
    }
  }

  function handleNewTransfer() {
    idempotencyKeyRef.current = crypto.randomUUID()
    setResult(null)
    setFraudUnavailable(false)
    setServerError(null)
    reset()
  }

  return (
    <Card>
      <h2>Перевод</h2>
      <form className={styles.form} onSubmit={handleSubmit(onSubmit)} noValidate>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="recipientIban">
            IBAN получателя
          </label>
          <Input id="recipientIban" autoComplete="off" {...register('recipientIban')} />
          {errors.recipientIban && <ErrorText>{errors.recipientIban.message}</ErrorText>}
        </div>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="amount">
            Сумма
          </label>
          <Input id="amount" inputMode="decimal" placeholder="100.00" {...register('amount')} />
          {errors.amount && <ErrorText>{errors.amount.message}</ErrorText>}
        </div>
        {serverError && <ErrorText>{serverError}</ErrorText>}
        <Button type="submit" loading={isSubmitting}>
          Отправить
        </Button>
      </form>

      {result && (
        <div className={styles.result}>
          {result.status === 'completed' && (
            <Banner variant="success">
              Перевод выполнен: <Money value={result.amount} currency="EUR" showSign={false} />.
            </Banner>
          )}
          {result.status === 'failed' && <Banner variant="danger">{failureReasonMessage(result.failure_reason)}</Banner>}
          {result.status === 'rejected' && (
            <Banner variant="warning">
              Перевод заблокирован системой безопасности в целях вашей защиты.{' '}
              {rejectedReasonClause(result.failure_reason)}
            </Banner>
          )}
          {result.status === 'pending' && fraudUnavailable && (
            <Banner variant="warning">
              Проверка безопасности временно недоступна. Попробуйте повторить перевод.
            </Banner>
          )}
          {result.status === 'pending' && !fraudUnavailable && result.message && (
            <Banner variant="warning">
              Перевод обрабатывается, статус неизвестен — проверьте историю переводов ниже.
            </Banner>
          )}
          {result.status === 'pending' && fraudUnavailable ? (
            <Button
              variant="secondary"
              type="button"
              disabled={isSubmitting}
              className={styles.newTransferButton}
              onClick={handleSubmit(onSubmit)}
            >
              Повторить
            </Button>
          ) : (
            <Button variant="secondary" type="button" className={styles.newTransferButton} onClick={handleNewTransfer}>
              Новый перевод
            </Button>
          )}
        </div>
      )}
    </Card>
  )
}
