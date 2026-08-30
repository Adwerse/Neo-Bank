import { useEffect, useRef, useState } from 'react'
import type { FormEvent } from 'react'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { Elements, PaymentElement, useElements, useStripe } from '@stripe/react-stripe-js'
import type { StripeError } from '@stripe/stripe-js'
import { Card } from '../../../shared/ui/Card'
import { Input } from '../../../shared/ui/Input'
import { Button } from '../../../shared/ui/Button'
import { ErrorText } from '../../../shared/ui/ErrorText'
import { Banner } from '../../../shared/ui/Banner'
import { Money } from '../../../shared/ui/Money'
import { isApiError } from '../../../shared/api-client/ApiError'
import { errorMessage } from '../../../shared/errorMessages'
import { useIsDesktop } from '../../../shared/ui/useIsDesktop'
import { useMe } from '../../accounts/useMe'
import { parseAmountToCents } from '../../transfers/money'
import { useFlashOnChange } from '../../../shared/ui/useFlashOnChange'
import { depositSchema, type DepositFormValues } from '../schemas'
import { createDeposit } from '../api'
import { stripePromise } from '../stripe'
import { getStripeAppearance } from '../stripeAppearance'
import { useDepositStatusPolling, type DepositPollOutcome } from '../useDepositStatusPolling'
import styles from './DepositForm.module.css'

// Stripe's own PaymentIntent last_payment_error.code vocabulary — a
// separate namespace from the backend's error codes (shared/errorMessages
// .ts), so it stays its own lookup rather than merging into that one:
// Stripe's own "insufficient_funds" (the card itself lacks funds) and the
// backend's ledger "insufficient_funds" (the sender's balance is too low)
// are the same code string with different meanings, and conflating them
// would show the wrong message for whichever one didn't win the merge.
const DECLINE_CODE_LABELS: Record<string, string> = {
  card_declined: 'The card was declined by the issuing bank.',
  insufficient_funds: 'Insufficient funds on the card.',
  expired_card: 'The card has expired.',
  incorrect_cvc: 'Incorrect CVC code.',
  processing_error: 'Payment processing error, please try again.',
}

function declineMessage(error: StripeError): string {
  if (error.code && DECLINE_CODE_LABELS[error.code]) return DECLINE_CODE_LABELS[error.code]
  return error.message ?? 'The payment was declined, please try a different card.'
}

type Step =
  | { kind: 'amount'; declineMessage?: string }
  | { kind: 'payment'; depositId: string; clientSecret: string }
  | { kind: 'processing'; depositId: string }

export function DepositForm() {
  const [step, setStep] = useState<Step>({ kind: 'amount' })
  const [serverError, setServerError] = useState<string | null>(null)

  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<DepositFormValues>({ resolver: zodResolver(depositSchema) })

  async function onSubmit(values: DepositFormValues) {
    setServerError(null)
    const amountCents = parseAmountToCents(values.amount)
    if (amountCents === null) {
      setServerError('Enter a valid amount')
      return
    }
    try {
      const result = await createDeposit(amountCents)
      setStep({ kind: 'payment', depositId: result.deposit_id, clientSecret: result.client_secret })
    } catch (err) {
      setServerError(isApiError(err) ? errorMessage(err.message) : 'Could not start the deposit, please try again')
    }
  }

  const isDesktop = useIsDesktop()

  if (step.kind === 'payment') {
    return (
      <Elements
        stripe={stripePromise}
        options={{ clientSecret: step.clientSecret, appearance: getStripeAppearance(isDesktop) }}
      >
        <PaymentStep
          onDeclined={(message) => setStep({ kind: 'amount', declineMessage: message })}
          onConfirmed={() => setStep({ kind: 'processing', depositId: step.depositId })}
          onCancel={() => setStep({ kind: 'amount' })}
        />
      </Elements>
    )
  }

  if (step.kind === 'processing') {
    return <ProcessingStep depositId={step.depositId} onStartOver={() => setStep({ kind: 'amount' })} />
  }

  return (
    <Card>
      <h2>Deposit funds</h2>
      <form className={styles.form} onSubmit={handleSubmit(onSubmit)} noValidate>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="amount">
            Deposit amount
          </label>
          <Input
            id="amount"
            inputMode="decimal"
            placeholder="0.00"
            className={styles.amountInput}
            autoFocus
            error={Boolean(errors.amount)}
            aria-describedby={errors.amount ? 'amount-error' : undefined}
            {...register('amount')}
          />
          {errors.amount && <ErrorText id="amount-error">{errors.amount.message}</ErrorText>}
        </div>
        {serverError && <ErrorText>{serverError}</ErrorText>}
        {step.declineMessage && <Banner variant="danger">{step.declineMessage}</Banner>}
        <Button type="submit" loading={isSubmitting}>
          Continue
        </Button>
      </form>
    </Card>
  )
}

interface PaymentStepProps {
  onDeclined: (message: string) => void
  onConfirmed: () => void
  onCancel: () => void
}

// Rendered as a child of <Elements> — useStripe/useElements only work
// inside that provider's tree, which is why this can't just live inline
// in DepositForm above.
function PaymentStep({ onDeclined, onConfirmed, onCancel }: PaymentStepProps) {
  const stripe = useStripe()
  const elements = useElements()
  const [isConfirming, setIsConfirming] = useState(false)
  // This step is a genuinely separate component instance each time it
  // mounts (DepositForm swaps its whole return value between steps), so a
  // plain mount-effect lands focus here exactly once per entry — same
  // reasoning as TransferForm's step-transition focus management.
  const headingRef = useRef<HTMLHeadingElement>(null)
  useEffect(() => {
    headingRef.current?.focus()
  }, [])

  async function handleConfirm(e: FormEvent) {
    e.preventDefault()
    if (!stripe || !elements) return
    setIsConfirming(true)

    // redirect: 'if_required' keeps this a single-page flow for ordinary
    // cards (including most 3D Secure challenges, handled inline by
    // Stripe.js) — return_url is still required by the API for the rare
    // payment method that forces a top-level redirect.
    const { error, paymentIntent } = await stripe.confirmPayment({
      elements,
      confirmParams: { return_url: `${window.location.origin}/deposit` },
      redirect: 'if_required',
    })

    setIsConfirming(false)

    if (error) {
      // A decline means this PaymentIntent is done — task requirement is
      // explicit: a retry must be a NEW deposit (new PaymentIntent), never
      // reusing this client_secret. Bouncing all the way back to the
      // amount step is what makes that the only option.
      onDeclined(declineMessage(error))
      return
    }
    if (paymentIntent && (paymentIntent.status === 'succeeded' || paymentIntent.status === 'processing')) {
      // Stripe accepted the payment — NOT the same as the ledger balance
      // having moved yet. See useDepositStatusPolling for what happens
      // next.
      onConfirmed()
      return
    }
    onDeclined('Could not confirm the payment, please try again.')
  }

  return (
    <Card>
      <h2 ref={headingRef} tabIndex={-1} className={styles.focusableHeading}>
        Pay by card
      </h2>
      <form className={styles.form} onSubmit={handleConfirm}>
        <PaymentElement />
        <div className={styles.paymentActions}>
          <Button type="submit" disabled={!stripe} loading={isConfirming}>
            Pay
          </Button>
          <Button type="button" variant="secondary" disabled={isConfirming} onClick={onCancel}>
            Change amount
          </Button>
        </div>
      </form>
    </Card>
  )
}

// Purely presentational — the honest "accepted -> processing -> credited"
// sequence itself still comes entirely from useDepositStatusPolling's
// outcome below; this just gives it a visible spine.
function DepositProgress({ outcome }: { outcome: DepositPollOutcome }) {
  const resolved = outcome === 'credited' || outcome === 'failed'
  const failed = outcome === 'failed'
  return (
    <ol className={styles.progress}>
      <li className={styles.progressDone}>Payment accepted</li>
      <li className={resolved ? styles.progressDone : styles.progressActive}>Processing</li>
      <li className={resolved ? (failed ? styles.progressFailed : styles.progressDone) : undefined}>
        {failed ? 'Not credited' : 'Credited'}
      </li>
    </ol>
  )
}

function ProcessingStep({ depositId, onStartOver }: { depositId: string; onStartOver: () => void }) {
  const { deposit, outcome } = useDepositStatusPolling(depositId)
  // The credited branch below wants the account's current total balance,
  // not just the deposited amount — deposit.updated already invalidates
  // this same ['accounts','me'] query (see WebSocketProvider), so by the
  // time outcome flips to 'credited' this is the fresh number, no extra
  // fetch triggered here.
  const { data: account, isError: accountError } = useMe()
  const balanceFlash = useFlashOnChange(account?.balance)

  // Same step-transition focus problem as PaymentStep, plus a second
  // case unique to this step: it stays mounted for the whole
  // polling -> credited/failed/timed_out span, so reaching a terminal
  // outcome doesn't remount anything — without this second effect a
  // keyboard/screen-reader user would never be told the wait is over.
  const headingRef = useRef<HTMLHeadingElement>(null)
  const resultRef = useRef<HTMLDivElement>(null)
  const isResolved = outcome === 'credited' || outcome === 'failed' || outcome === 'timed_out'

  useEffect(() => {
    headingRef.current?.focus()
  }, [])

  useEffect(() => {
    if (isResolved) resultRef.current?.focus()
  }, [isResolved])

  return (
    <Card>
      <h2 ref={headingRef} tabIndex={-1} className={styles.focusableHeading}>
        Deposit funds
      </h2>
      <DepositProgress outcome={outcome} />
      <div className={styles.result} ref={resultRef} tabIndex={-1}>
        {outcome === 'polling' && <Banner variant="warning">Payment accepted, crediting within a minute…</Banner>}
        {outcome === 'timed_out' && (
          <Banner variant="warning">Crediting is taking longer than usual — please check your balance later.</Banner>
        )}
        {outcome === 'failed' && (
          <Banner variant="danger">The payment was not credited. Please start a new deposit.</Banner>
        )}
        {outcome === 'credited' && deposit && (
          <>
            <Banner variant="success">
              Balance topped up by{' '}
              <Money value={deposit.amount} currency="EUR" showSign={false} label="Deposit amount" />.
            </Banner>
            {account && (
              <div className={styles.currentBalance}>
                Current balance:{' '}
                <Money
                  value={account.balance}
                  currency={account.currency}
                  showSign={false}
                  label="Current balance"
                  className={balanceFlash ? styles.balanceChanged : undefined}
                />
              </div>
            )}
            {!account && accountError && (
              <div className={styles.currentBalance}>Balance temporarily unavailable — please refresh later.</div>
            )}
          </>
        )}
        {(outcome === 'credited' || outcome === 'failed' || outcome === 'timed_out') && (
          <Button variant="secondary" type="button" className={styles.newDepositButton} onClick={onStartOver}>
            New deposit
          </Button>
        )}
      </div>
    </Card>
  )
}
