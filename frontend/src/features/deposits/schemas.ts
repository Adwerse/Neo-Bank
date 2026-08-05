import { z } from 'zod'
import { parseAmountToCents } from '../transfers/money'

// Mirrors transferSchema (features/transfers/schemas.ts) exactly for the
// amount field — same cents normalization, same regex — per the task's
// own requirement to reuse the transfer form's normalization rather than
// reimplementing it. depositMinAmount/depositMaxAmount
// (services/transfers-svc/deposit.go) bound this server-side too; this is
// just the client-side UX check for the same rule (€0.50–€10,000.00).
const MIN_CENTS = 50
const MAX_CENTS = 1_000_000

export const depositSchema = z.object({
  amount: z
    .string()
    .regex(/^\d+([.,]\d{1,2})?$/, 'Введите сумму, например 100 или 99.50')
    .refine((v) => {
      const cents = parseAmountToCents(v)
      return cents !== null && cents >= MIN_CENTS
    }, 'Минимальная сумма пополнения — 0.50 €')
    .refine((v) => {
      const cents = parseAmountToCents(v)
      return cents !== null && cents <= MAX_CENTS
    }, 'Максимальная сумма пополнения — 10 000.00 €'),
})

export type DepositFormValues = z.infer<typeof depositSchema>
