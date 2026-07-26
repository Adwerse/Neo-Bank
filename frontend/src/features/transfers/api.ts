import { request } from '../../shared/api-client/client'
import type { paths } from '../../shared/api-client/schema'

type CreateTransferBody = paths['/transfers']['post']['requestBody']['content']['application/json']
type TransferResult = paths['/transfers']['post']['responses']['201']['content']['application/json']
type TransferHistoryEntry = paths['/transfers']['get']['responses']['200']['content']['application/json'][number]

// The trailing slash on '/transfers/' is load-bearing: gateway/proxy.go
// mounts this route as a subtree pattern ("/transfers/"), and every OTHER
// proxied endpoint the frontend calls has a path segment after the prefix
// (e.g. /accounts/me), so this is the first call to hit the bare prefix.
// Go's http.ServeMux 301-redirects "/transfers" -> "/transfers/" for a
// subtree pattern, and fetch does not resend a POST body across that
// redirect — calling the prefix with its trailing slash up front avoids
// the redirect entirely.

// No skipAuthRetry: a transfer only ever happens with an existing session,
// so a 401 here is a stale-token signal, not a business answer — same
// reasoning as auth's logout(). It should go through the normal
// refresh-and-retry.
export function createTransfer(body: CreateTransferBody, idempotencyKey: string): Promise<TransferResult> {
  return request<TransferResult>('/transfers/', {
    method: 'POST',
    body: JSON.stringify(body),
    headers: { 'Idempotency-Key': idempotencyKey },
  })
}

export function listTransfers(params?: { limit?: number; offset?: number }): Promise<TransferHistoryEntry[]> {
  const query = new URLSearchParams()
  if (params?.limit) query.set('limit', String(params.limit))
  if (params?.offset) query.set('offset', String(params.offset))
  const qs = query.toString()
  return request<TransferHistoryEntry[]>(`/transfers/${qs ? `?${qs}` : ''}`)
}

export type { TransferResult, TransferHistoryEntry }
