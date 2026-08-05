import { useQuery } from '@tanstack/react-query'
import { listTransfers } from './api'

export function useOperationHistory() {
  return useQuery({
    queryKey: ['transfers', 'history'],
    queryFn: () => listTransfers({ limit: 20 }),
  })
}
