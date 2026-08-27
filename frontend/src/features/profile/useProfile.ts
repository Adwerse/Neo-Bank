import { useQuery } from '@tanstack/react-query'
import { getProfile } from './api'

// No WS push exists for profile changes (WebSocketProvider only handles
// balance.changed/transfer.updated/deposit.updated) — a display-name or
// avatar change only ever happens from this same client's own mutation,
// which already writes the fresh result straight into this query's cache
// (see useUpdateDisplayName and useAvatarUpload), so there's nothing a
// background poll would catch that those don't already cover.
export function useProfile() {
  return useQuery({
    queryKey: ['profile'],
    queryFn: getProfile,
  })
}
