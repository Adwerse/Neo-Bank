import { request } from '../../shared/api-client/client'
import type { paths } from '../../shared/api-client/schema'

type CreateTransferBody = paths['/transfers']['post']['requestBody']['content']['application/json']
type TransferResult = paths['/transfers']['post']['responses']['201']['content']['application/json']
type TransferHistoryPage = paths['/transfers']['get']['responses']['200']['content']['application/json']
// GET /transfers is the caller's UNIFIED operations feed (transfers,
// deposits, and withdrawals interleaved by time, each tagged with `type`)
// despite the name — see services/transfers-svc/history.go's historyEntry
// doc comment for why the endpoint/field name stayed as-is.
type OperationHistoryEntry = TransferHistoryPage['transfers'][number]

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

// listOperationHistoryPage returns the raw {transfers, next_cursor} page —
// the "load more" / infinite-scroll case, which needs next_cursor to fetch
// the following page. cursor is opaque, from a previous page's next_cursor;
// omit for the first page.
export function listOperationHistoryPage(params?: { limit?: number; cursor?: string }): Promise<TransferHistoryPage> {
  const query = new URLSearchParams()
  if (params?.limit) query.set('limit', String(params.limit))
  if (params?.cursor) query.set('cursor', params.cursor)
  const qs = query.toString()
  return request<TransferHistoryPage>(`/transfers/${qs ? `?${qs}` : ''}`)
}

// listTransfers unwraps the {transfers, next_cursor} envelope into a bare
// array, for callers that only ever want the first page (dashboard
// previews) and have no use for next_cursor.
export function listTransfers(params?: { limit?: number; cursor?: string }): Promise<OperationHistoryEntry[]> {
  return listOperationHistoryPage(params).then((page) => page.transfers)
}

export type { TransferResult, OperationHistoryEntry, TransferHistoryPage }
