import { z } from 'zod'

export const registerSchema = z
  .object({
    email: z.email('Введите корректный email'),
    password: z.string().min(8, 'Пароль должен содержать не менее 8 символов'),
    confirmPassword: z.string(),
  })
  .refine((data) => data.password === data.confirmPassword, {
    message: 'Пароли не совпадают',
    path: ['confirmPassword'],
  })

export type RegisterFormValues = z.infer<typeof registerSchema>

export const verifyCodeSchema = z.object({
  code: z.string().regex(/^\d{6}$/, 'Код должен состоять из 6 цифр'),
})

export type VerifyCodeFormValues = z.infer<typeof verifyCodeSchema>

export const loginSchema = z.object({
  email: z.email('Введите корректный email'),
  password: z.string().min(1, 'Введите пароль'),
})

export type LoginFormValues = z.infer<typeof loginSchema>

export const forgotPasswordSchema = z.object({
  email: z.email('Введите корректный email'),
})

export type ForgotPasswordFormValues = z.infer<typeof forgotPasswordSchema>

export const resetPasswordSchema = z
  .object({
    email: z.email('Введите корректный email'),
    code: z.string().regex(/^\d{6}$/, 'Код должен состоять из 6 цифр'),
    newPassword: z.string().min(8, 'Пароль должен содержать не менее 8 символов'),
    confirmNewPassword: z.string(),
  })
  .refine((data) => data.newPassword === data.confirmNewPassword, {
    message: 'Пароли не совпадают',
    path: ['confirmNewPassword'],
  })

export type ResetPasswordFormValues = z.infer<typeof resetPasswordSchema>
