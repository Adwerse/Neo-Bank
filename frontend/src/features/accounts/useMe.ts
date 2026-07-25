import { useQuery } from '@tanstack/react-query'
import { getMe } from './api'

export function useMe() {
  return useQuery({
    queryKey: ['accounts', 'me'],
    queryFn: getMe,
    // A predictable manual "Повторить" button beats silent background
    // retries on a balance screen — the user should see the failure and
    // choose to retry, not wait through a hidden backoff.
    retry: false,
  })
}
