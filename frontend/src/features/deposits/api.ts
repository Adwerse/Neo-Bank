import { request } from '../../shared/api-client/client'
import type { paths } from '../../shared/api-client/schema'

type CreateDepositBody = paths['/deposits']['post']['requestBody']['content']['application/json']
type CreateDepositResult = paths['/deposits']['post']['responses']['201']['content']['application/json']
type Deposit = paths['/deposits/{id}']['get']['responses']['200']['content']['application/json']

// No trailing slash, unlike '/transfers/': the Gateway registers
// "/deposits" as its own exact pattern specifically so this plain path
// never hits ServeMux's auto-redirect-to-subtree behavior (see
// gateway/main.go's newHandler comment) — a 301 on this POST would have
// fetch() silently drop the body on retry.
export function createDeposit(amount: number): Promise<CreateDepositResult> {
  return request<CreateDepositResult>('/deposits', {
    method: 'POST',
    body: JSON.stringify({ amount } satisfies CreateDepositBody),
  })
}

export function getDeposit(id: string): Promise<Deposit> {
  return request<Deposit>(`/deposits/${id}`)
}

export type { CreateDepositResult, Deposit }
