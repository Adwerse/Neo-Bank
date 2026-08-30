import { z } from 'zod'
import { parseAmountToCents } from './money'
import { validateIban } from './iban'

export const transferSchema = z.object({
  recipientIban: z
    .string()
    .min(1, "Enter the recipient's IBAN")
    .refine((v: string) => validateIban(v) === null, "Check the recipient's IBAN — invalid format or check digits"),
  amount: z
    .string()
    .regex(/^\d+([.,]\d{1,2})?$/, 'Enter an amount, e.g. 100 or 99.50')
    .refine((v) => (parseAmountToCents(v) ?? 0) > 0, 'Amount must be greater than zero'),
})

export type TransferFormValues = z.infer<typeof transferSchema>
