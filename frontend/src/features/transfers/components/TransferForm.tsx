import { useEffect, useRef, useState } from 'react'
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
import { errorMessage, REJECTED_REASON_CLAUSES } from '../../../shared/errorMessages'
import { useMe } from '../../accounts/useMe'
import { createTransfer, type TransferResult } from '../api'
import { parseAmountToCents } from '../money'
import { transferSchema, type TransferFormValues } from '../schemas'
import { IbanField } from './IbanField'
import styles from './TransferForm.module.css'

function failureReasonMessage(reason?: string): string {
  if (!reason) return 'Transfer failed'
  return errorMessage(reason, 'Transfer failed')
}

// failure_reason holds two disjoint vocabularies depending on status:
// ledger codes when failed (looked up via errorMessage above, full
// sentences), fraud-svc's triggered rule name when rejected (looked up
// here, lowercase clause fragments for the "Reason: ..." template below)
// — see services/transfers-svc/transfer.go's Transfer.FailureReason doc
// comment. Deliberately does NOT fall back to the raw code: an
// unrecognized rule name is an internal implementation detail, not
// something to show a user, and must never leak here.
function rejectedReasonClause(reason?: string): string {
  const clause = reason ? REJECTED_REASON_CLAUSES[reason] : undefined
  return clause ? `Reason: ${clause}.` : ''
}

// The only signal distinguishing this 202 cause from the pre-existing
// ledger-uncertain one: both share the identical TransferResult shape
// (status "pending" + optional message), so the message text itself is
// the discriminator. Matches services/transfers-svc/http.go's literal —
// this is the Transfer's own `message` field on a 202 response, not an
// Error-schema `error` code, so it was never part of the error-code
// refactor.
const FRAUD_CHECK_UNAVAILABLE_MESSAGE = 'fraud check unavailable, transfer still pending'

export function TransferForm() {
  const queryClient = useQueryClient()
  const { data: account, isLoading: accountLoading, isError: accountError } = useMe()

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
  // fraud-unavailable banner/button and fall back to "New transfer", even
  // though nothing was actually resolved.
  const [fraudUnavailable, setFraudUnavailable] = useState(false)
  // Purely a presentational gate in front of the real submit — a review
  // step the user must confirm before onSubmit ever runs. Doesn't change
  // when the idempotency key is generated (still at mount, above) or when
  // the API is actually called (still only from handleConfirm/Retry).
  const [reviewValues, setReviewValues] = useState<TransferFormValues | null>(null)

  // The review and result "steps" are different JSX trees rendered by the
  // same component instance, not separate routes — a browser drops focus
  // to <body> when the previously-focused element (the "Continue"/
  // "Confirm" button) is removed from the DOM by that swap, which would
  // strand a keyboard user back at the top of the page. These two effects
  // re-land focus on the new step's primary element instead.
  const reviewButtonRef = useRef<HTMLButtonElement>(null)
  const resultRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (reviewValues && !result) reviewButtonRef.current?.focus()
  }, [reviewValues, result])

  useEffect(() => {
    if (result) resultRef.current?.focus()
  }, [result])

  const {
    control,
    register,
    handleSubmit,
    reset,
    formState: { errors, isSubmitting },
  } = useForm<TransferFormValues>({ resolver: zodResolver(transferSchema) })

  async function onSubmit(values: TransferFormValues) {
    setServerError(null)
    const amountCents = parseAmountToCents(values.amount)
    if (amountCents === null) {
      setServerError('Enter a valid amount')
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
      setServerError(isApiError(err) ? errorMessage(err.message) : 'Could not complete the transfer, please try again')
    }
  }

  function handleReview(values: TransferFormValues) {
    setServerError(null)
    setReviewValues(values)
  }

  function handleBackToForm() {
    setServerError(null)
    setReviewValues(null)
  }

  function handleNewTransfer() {
    idempotencyKeyRef.current = crypto.randomUUID()
    setResult(null)
    setFraudUnavailable(false)
    setServerError(null)
    setReviewValues(null)
    reset()
  }

  // Review step: values are already schema-valid (react-hook-form only
  // calls handleReview once validation passes) — this is a confirmation
  // screen, not a second validation pass.
  if (reviewValues && !result) {
    const amountCents = parseAmountToCents(reviewValues.amount) ?? 0
    return (
      <Card>
        <h2>Confirm transfer</h2>
        <div className={styles.review}>
          <div className={styles.reviewRow}>
            <span className={styles.reviewLabel}>Recipient</span>
            <span className={styles.reviewIban}>{reviewValues.recipientIban}</span>
          </div>
          <div className={styles.reviewRow}>
            <span className={styles.reviewLabel}>Sending</span>
            <Money value={-amountCents} currency="EUR" size="hero" label="Sending" />
          </div>
          {account && (
            <div className={styles.reviewRow}>
              <span className={styles.reviewLabel}>Remaining balance</span>
              <Money
                value={account.balance - amountCents}
                currency={account.currency}
                showSign={false}
                tone="neutral"
                label="Remaining balance"
              />
            </div>
          )}
          {serverError && <ErrorText>{serverError}</ErrorText>}
          <div className={styles.reviewActions}>
            <Button ref={reviewButtonRef} type="button" loading={isSubmitting} onClick={() => onSubmit(reviewValues)}>
              Confirm and send
            </Button>
            <Button type="button" variant="secondary" disabled={isSubmitting} onClick={handleBackToForm}>
              Edit
            </Button>
          </div>
        </div>
      </Card>
    )
  }

  return (
    <Card>
      <h2>Transfer</h2>
      {!result && (
        <form className={styles.form} onSubmit={handleSubmit(handleReview)} noValidate>
          <div className={styles.field}>
            <label className={styles.label} htmlFor="recipientIban">
              Recipient's IBAN
            </label>
            <IbanField control={control} />
          </div>
          <div className={styles.field}>
            <div className={styles.amountLabelRow}>
              <label className={styles.label} htmlFor="amount">
                Amount
              </label>
              {!accountLoading && !accountError && account && (
                <span className={styles.available}>
                  Available:{' '}
                  <Money value={account.balance} currency={account.currency} showSign={false} tone="faint" label="Available" />
                </span>
              )}
              {accountError && <span className={styles.available}>Balance temporarily unavailable</span>}
            </div>
            <Input
              id="amount"
              inputMode="decimal"
              placeholder="0.00"
              className={styles.amountInput}
              error={Boolean(errors.amount)}
              aria-describedby={errors.amount ? 'amount-error' : undefined}
              {...register('amount')}
            />
            {errors.amount && <ErrorText id="amount-error">{errors.amount.message}</ErrorText>}
          </div>
          {serverError && <ErrorText>{serverError}</ErrorText>}
          <Button type="submit" loading={isSubmitting}>
            Continue
          </Button>
        </form>
      )}

      {result && (
        <div className={styles.result} ref={resultRef} tabIndex={-1}>
          {result.status === 'completed' && (
            <Banner variant="success">
              Transfer completed:{' '}
              <Money value={result.amount} currency="EUR" showSign={false} label="Transfer amount" />.
            </Banner>
          )}
          {result.status === 'failed' && <Banner variant="danger">{failureReasonMessage(result.failure_reason)}</Banner>}
          {result.status === 'rejected' && (
            <Banner variant="warning">
              This transfer was blocked by our security system to protect you.{' '}
              {rejectedReasonClause(result.failure_reason)}
            </Banner>
          )}
          {result.status === 'pending' && fraudUnavailable && (
            <Banner variant="warning">Security check temporarily unavailable. Please try the transfer again.</Banner>
          )}
          {result.status === 'pending' && !fraudUnavailable && result.message && (
            <Banner variant="warning">Transfer is processing, status unknown — check the operation history below.</Banner>
          )}
          {result.status === 'pending' && fraudUnavailable ? (
            <Button
              variant="secondary"
              type="button"
              loading={isSubmitting}
              className={styles.newTransferButton}
              onClick={() => reviewValues && onSubmit(reviewValues)}
            >
              Retry
            </Button>
          ) : (
            <Button variant="secondary" type="button" className={styles.newTransferButton} onClick={handleNewTransfer}>
              New transfer
            </Button>
          )}
        </div>
      )}
    </Card>
  )
}
