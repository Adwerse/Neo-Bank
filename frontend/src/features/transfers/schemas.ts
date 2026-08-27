import { z } from 'zod'
import { parseAmountToCents } from './money'
import { validateIban } from './iban'

export const transferSchema = z.object({
  recipientIban: z
    .string()
    .min(1, 'Введите IBAN получателя')
    .refine((v: string) => validateIban(v) === null, 'Проверьте IBAN получателя — неверный формат или контрольные цифры'),
  amount: z
    .string()
    .regex(/^\d+([.,]\d{1,2})?$/, 'Введите сумму, например 100 или 99.50')
    .refine((v) => (parseAmountToCents(v) ?? 0) > 0, 'Сумма должна быть больше нуля'),
})

export type TransferFormValues = z.infer<typeof transferSchema>
