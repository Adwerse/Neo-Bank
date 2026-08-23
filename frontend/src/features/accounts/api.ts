import { request } from '../../shared/api-client/client'
import type { paths } from '../../shared/api-client/schema'

type MeResponse = paths['/accounts/me']['get']['responses']['200']['content']['application/json']
type BalanceHistoryResponse =
  paths['/accounts/me/balance-history']['get']['responses']['200']['content']['application/json']
type BalanceHistoryRange = NonNullable<
  paths['/accounts/me/balance-history']['get']['parameters']['query']
>['range']

export function getMe(): Promise<MeResponse> {
  return request<MeResponse>('/accounts/me')
}

export function getBalanceHistory(range: BalanceHistoryRange): Promise<BalanceHistoryResponse> {
  return request<BalanceHistoryResponse>(`/accounts/me/balance-history?range=${range}`)
}

export type { MeResponse, BalanceHistoryResponse, BalanceHistoryRange }
