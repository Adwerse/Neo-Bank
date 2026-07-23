import { request } from '../../shared/api-client/client'
import type { paths } from '../../shared/api-client/schema'

type MeResponse = paths['/accounts/me']['get']['responses']['200']['content']['application/json']

export function getMe(): Promise<MeResponse> {
  return request<MeResponse>('/accounts/me')
}
