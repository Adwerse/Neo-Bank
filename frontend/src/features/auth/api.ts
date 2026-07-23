import { request } from '../../shared/api-client/client'
import type { paths } from '../../shared/api-client/schema'

type RegisterBody = paths['/auth/register']['post']['requestBody']['content']['application/json']
type RegisterResponse = paths['/auth/register']['post']['responses']['201']['content']['application/json']

type VerifyEmailBody = paths['/auth/verify-email']['post']['requestBody']['content']['application/json']
type VerifyEmailResponse = paths['/auth/verify-email']['post']['responses']['200']['content']['application/json']

type ResendVerificationBody =
  paths['/auth/resend-verification']['post']['requestBody']['content']['application/json']
type ResendVerificationResponse =
  paths['/auth/resend-verification']['post']['responses']['200']['content']['application/json']

type LoginBody = paths['/auth/login']['post']['requestBody']['content']['application/json']
type LoginResponse = paths['/auth/login']['post']['responses']['200']['content']['application/json']

type LogoutBody = paths['/auth/logout']['post']['requestBody']['content']['application/json']
type LogoutResponse = paths['/auth/logout']['post']['responses']['200']['content']['application/json']

type ForgotPasswordBody = paths['/auth/forgot-password']['post']['requestBody']['content']['application/json']
type ForgotPasswordResponse =
  paths['/auth/forgot-password']['post']['responses']['200']['content']['application/json']

type ResetPasswordBody = paths['/auth/reset-password']['post']['requestBody']['content']['application/json']
type ResetPasswordResponse =
  paths['/auth/reset-password']['post']['responses']['200']['content']['application/json']

// register/verifyEmail/resendVerification/login/forgotPassword/resetPassword
// all run before a session exists, so a 401 from any of them (login is the
// only one that can actually return one) is a business answer — wrong
// credentials — not a stale-access-token signal. skipAuthRetry keeps the
// shared client's refresh-and-retry machinery from misfiring on them.
//
// logout is the one auth endpoint that genuinely requires a session
// (gateway/middleware.go doesn't list it as public), so it's the one call
// here that's allowed to go through the normal refresh-on-401 path.

export function register(body: RegisterBody): Promise<RegisterResponse> {
  return request<RegisterResponse>(
    '/auth/register',
    { method: 'POST', body: JSON.stringify(body) },
    { skipAuthRetry: true },
  )
}

export function verifyEmail(body: VerifyEmailBody): Promise<VerifyEmailResponse> {
  return request<VerifyEmailResponse>(
    '/auth/verify-email',
    { method: 'POST', body: JSON.stringify(body) },
    { skipAuthRetry: true },
  )
}

export function resendVerification(body: ResendVerificationBody): Promise<ResendVerificationResponse> {
  return request<ResendVerificationResponse>(
    '/auth/resend-verification',
    { method: 'POST', body: JSON.stringify(body) },
    { skipAuthRetry: true },
  )
}

export function login(body: LoginBody): Promise<LoginResponse> {
  return request<LoginResponse>(
    '/auth/login',
    { method: 'POST', body: JSON.stringify(body) },
    { skipAuthRetry: true },
  )
}

export function logout(body: LogoutBody): Promise<LogoutResponse> {
  return request<LogoutResponse>('/auth/logout', { method: 'POST', body: JSON.stringify(body) })
}

export function forgotPassword(body: ForgotPasswordBody): Promise<ForgotPasswordResponse> {
  return request<ForgotPasswordResponse>(
    '/auth/forgot-password',
    { method: 'POST', body: JSON.stringify(body) },
    { skipAuthRetry: true },
  )
}

export function resetPassword(body: ResetPasswordBody): Promise<ResetPasswordResponse> {
  return request<ResetPasswordResponse>(
    '/auth/reset-password',
    { method: 'POST', body: JSON.stringify(body) },
    { skipAuthRetry: true },
  )
}
