import { useRef, useState } from 'react'
import type { ChangeEvent } from 'react'
import { Avatar } from '../../../shared/ui/Avatar'
import { Banner } from '../../../shared/ui/Banner'
import { ErrorText } from '../../../shared/ui/ErrorText'
import { CameraIcon } from '../../../shared/ui/icons'
import { useToast } from '../../../shared/ui/toast/ToastProvider'
import { getAccessTokenEmail } from '../../../shared/api-client/jwt'
import { getDisplayName } from '../../accounts/displayName'
import { validateAvatarFile, useAvatarUpload } from '../useAvatarUpload'
import type { Profile } from '../api'
import { AvatarCropModal } from './AvatarCropModal'
import styles from './AvatarUploader.module.css'

type LocalStep = { kind: 'idle' } | { kind: 'cropping'; file: File }

export function AvatarUploader({ profile }: { profile: Profile }) {
  const [localStep, setLocalStep] = useState<LocalStep>({ kind: 'idle' })
  const [localError, setLocalError] = useState<string | null>(null)
  const { step, upload, reset } = useAvatarUpload()
  const { showToast } = useToast()
  const inputRef = useRef<HTMLInputElement>(null)

  const fallback = getDisplayName(getAccessTokenEmail())
  const displayName = profile.display_name?.trim() || fallback.name
  const initial = displayName[0]?.toUpperCase() ?? fallback.initial
  const isBusy = step.kind === 'uploading' || step.kind === 'confirming'

  function handleFileChange(e: ChangeEvent<HTMLInputElement>) {
    const file = e.target.files?.[0]
    // Reset the input so selecting the exact same file again later (e.g.
    // after cancelling the crop) still fires a change event.
    e.target.value = ''
    if (!file) return

    const validationError = validateAvatarFile(file)
    if (validationError) {
      setLocalError(validationError)
      return
    }
    setLocalError(null)
    reset()
    setLocalStep({ kind: 'cropping', file })
  }

  function handleCancelCrop() {
    setLocalStep({ kind: 'idle' })
  }

  async function handleCropped(blob: Blob) {
    setLocalStep({ kind: 'idle' })
    const ok = await upload(blob)
    if (ok) showToast('Аватар обновлён', 'success')
  }

  return (
    <div className={styles.wrapper}>
      <div className={styles.avatarBox}>
        <Avatar imageUrl={profile.avatar_url_256} seed={profile.user_id} initial={initial} size={96} />
        <button
          type="button"
          className={styles.cameraButton}
          onClick={() => inputRef.current?.click()}
          disabled={isBusy}
          aria-label="Изменить аватар"
        >
          <CameraIcon size={16} />
        </button>
        <input
          ref={inputRef}
          type="file"
          accept="image/jpeg,image/png"
          className={styles.hiddenInput}
          onChange={handleFileChange}
          aria-hidden="true"
          tabIndex={-1}
        />
      </div>

      {isBusy && (
        <div className={styles.progress} role="status">
          <div className={styles.progressTrack}>
            <div
              className={styles.progressFill}
              style={{ width: step.kind === 'uploading' ? `${Math.round(step.progress * 100)}%` : '100%' }}
            />
          </div>
          <span className={styles.progressLabel}>
            {step.kind === 'uploading' ? `Загрузка… ${Math.round(step.progress * 100)}%` : 'Подтверждение…'}
          </span>
        </div>
      )}

      {localError && <ErrorText>{localError}</ErrorText>}
      {step.kind === 'error' && <Banner variant="danger">{step.message}</Banner>}

      {localStep.kind === 'cropping' && (
        <AvatarCropModal file={localStep.file} onCancel={handleCancelCrop} onCropped={handleCropped} />
      )}
    </div>
  )
}
