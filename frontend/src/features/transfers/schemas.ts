import { z } from 'zod'
import { parseAmountToCents } from './money'

export const transferSchema = z.object({
  recipientAccountNumber: z.string().min(1, 'Введите номер счёта получателя'),
  amount: z
    .string()
    .regex(/^\d+([.,]\d{1,2})?$/, 'Введите сумму, например 100 или 99.50')
    .refine((v) => (parseAmountToCents(v) ?? 0) > 0, 'Сумма должна быть больше нуля'),
})

export type TransferFormValues = z.infer<typeof transferSchema>
