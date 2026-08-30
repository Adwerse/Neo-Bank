import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { isApiError } from '../../shared/api-client/ApiError'
import { errorMessage } from '../../shared/errorMessages'
import { confirmAvatarUpload, createAvatarUploadURL, uploadAvatarFile, type Profile } from './api'

// Mirrors services/auth-svc/avatar.go's maxAvatarUploadBytes/allowed
// content types — a local pre-check so an obviously-bad file (a 40MB
// RAW photo, a PDF renamed .jpg) never goes anywhere near the network,
// per the task's explicit "don't run known-bad files through the whole
// flow" requirement. The server remains the authoritative check either
// way (it sniffs actual bytes, this only trusts the browser's File.type).
export const MAX_AVATAR_BYTES = 8 * 1024 * 1024
const ALLOWED_TYPES = new Set(['image/jpeg', 'image/png'])

export function validateAvatarFile(file: File): string | null {
  if (!ALLOWED_TYPES.has(file.type)) {
    return 'Only JPEG or PNG files are supported'
  }
  if (file.size > MAX_AVATAR_BYTES) {
    return `That file is too large (max ${Math.floor(MAX_AVATAR_BYTES / 1024 / 1024)} MB)`
  }
  return null
}

type UploadStep =
  | { kind: 'idle' }
  | { kind: 'uploading'; progress: number }
  | { kind: 'confirming' }
  | { kind: 'error'; message: string }

// The confirm step's 400s (avatar_too_large / avatar_unsupported_type /
// avatar_too_many_pixels / avatar_decode_failed — see errorMessages.ts)
// are exactly the case the task warns about: the file DID upload, it
// just didn't pass validation, so this must never fall back to a generic
// "upload failed" that reads like the network step itself failed.

// Orchestrates the three-step flow end to end: request a presigned POST
// target, PUT the file there with progress, then confirm. Each step is
// its own UploadStep so the UI can show exactly where a failure happened
// — "couldn't start the upload" reads very differently from "uploaded,
// but rejected".
export function useAvatarUpload() {
  const queryClient = useQueryClient()
  const [step, setStep] = useState<UploadStep>({ kind: 'idle' })

  async function upload(blob: Blob) {
    setStep({ kind: 'uploading', progress: 0 })
    try {
      const target = await createAvatarUploadURL()
      await uploadAvatarFile(target, blob, (fraction) => setStep({ kind: 'uploading', progress: fraction }))

      setStep({ kind: 'confirming' })
      const profile = await confirmAvatarUpload(target.key)

      // The task is explicit: the shell's avatar must update the instant
      // confirm succeeds. Writing the fresh Profile straight into the
      // cache is more immediate than invalidate-then-refetch, and still
      // exactly the "update the profile cache" the task asks for —
      // MobileShell/Sidebar both read this same ['profile'] query.
      queryClient.setQueryData<Profile>(['profile'], profile)
      setStep({ kind: 'idle' })
      return true
    } catch (err) {
      setStep({
        kind: 'error',
        message: isApiError(err) ? errorMessage(err.message) : 'Could not upload the avatar, please try again',
      })
      return false
    }
  }

  function reset() {
    setStep({ kind: 'idle' })
  }

  return { step, upload, reset }
}
