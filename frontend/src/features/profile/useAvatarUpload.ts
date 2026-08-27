import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { isApiError } from '../../shared/api-client/ApiError'
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
    return 'Поддерживаются только файлы JPEG или PNG'
  }
  if (file.size > MAX_AVATAR_BYTES) {
    return `Файл слишком большой (макс. ${Math.floor(MAX_AVATAR_BYTES / 1024 / 1024)} МБ)`
  }
  return null
}

type UploadStep =
  | { kind: 'idle' }
  | { kind: 'uploading'; progress: number }
  | { kind: 'confirming' }
  | { kind: 'error'; message: string }

const API_ERROR_LABELS: Record<string, string> = {
  'too many avatar upload requests, try again later': 'Слишком много попыток, попробуйте позже',
  'avatar upload not found': 'Загрузка не найдена — возможно, истекло время. Попробуйте снова.',
  'invalid key': 'Что-то пошло не так, попробуйте загрузить фото заново',
}

// The confirm step's 400s carry the server's own message straight through
// (e.g. 'unsupported image type "image/gif"', 'image is 6000x6000
// (36000000 pixels), want 20000000 or fewer') — those are already
// specific enough to show as-is, and re-explaining exactly why a
// server-side-detected file failed as a generic "upload failed" would be
// the wrong call the task explicitly warns about: the file DID upload,
// it just didn't pass validation.
function avatarErrorMessage(message: string): string {
  return API_ERROR_LABELS[message] ?? message
}

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
        message: isApiError(err)
          ? avatarErrorMessage(err.message)
          : 'Не удалось загрузить аватар, попробуйте ещё раз',
      })
      return false
    }
  }

  function reset() {
    setStep({ kind: 'idle' })
  }

  return { step, upload, reset }
}
