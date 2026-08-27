import { useMutation, useQueryClient } from '@tanstack/react-query'
import { updateProfile, type Profile } from './api'

// This app's first optimistic mutation — no existing onMutate pattern to
// follow, so the shape here is the textbook react-query one: snapshot the
// previous cache value in onMutate, apply the optimistic write, restore
// the snapshot in onError, and always resync from the server in
// onSettled. An optimistic update with no rollback is worse than no
// optimistic update at all — the user would see a name that never
// actually saved with nothing telling them so.
export function useUpdateDisplayName() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: (displayName: string) => updateProfile({ display_name: displayName }),
    onMutate: async (displayName: string) => {
      await queryClient.cancelQueries({ queryKey: ['profile'] })
      const previous = queryClient.getQueryData<Profile>(['profile'])
      queryClient.setQueryData<Profile>(['profile'], (current) =>
        current ? { ...current, display_name: displayName.trim() || null } : current,
      )
      return { previous }
    },
    onError: (_err, _displayName, context) => {
      if (context?.previous) {
        queryClient.setQueryData(['profile'], context.previous)
      }
    },
    onSuccess: (profile) => {
      // The server's version is authoritative (it may differ subtly from
      // the optimistic guess — e.g. trimming) — write it straight in
      // rather than waiting on the onSettled refetch below.
      queryClient.setQueryData(['profile'], profile)
    },
    onSettled: () => {
      queryClient.invalidateQueries({ queryKey: ['profile'] })
    },
  })
}
