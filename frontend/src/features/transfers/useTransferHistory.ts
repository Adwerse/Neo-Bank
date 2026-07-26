import { useQuery } from '@tanstack/react-query'
import { listTransfers } from './api'

export function useTransferHistory() {
  return useQuery({
    queryKey: ['transfers', 'history'],
    queryFn: () => listTransfers({ limit: 20 }),
  })
}
