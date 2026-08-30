import { useRef, useState } from 'react'
import type { KeyboardEvent } from 'react'
import { Button } from '../../../shared/ui/Button'
import { ErrorText } from '../../../shared/ui/ErrorText'
import { Input } from '../../../shared/ui/Input'
import { PencilIcon } from '../../../shared/ui/icons'
import { isApiError } from '../../../shared/api-client/ApiError'
import { errorMessage } from '../../../shared/errorMessages'
import { getAccessTokenEmail } from '../../../shared/api-client/jwt'
import { getDisplayName } from '../../accounts/displayName'
import { validateDisplayName } from '../displayNameValidation'
import { useUpdateDisplayName } from '../useUpdateDisplayName'
import type { Profile } from '../api'
import styles from './DisplayNameEditor.module.css'

export function DisplayNameEditor({ profile }: { profile: Profile }) {
  // null = not editing. A separate local string (not just derived from
  // the query cache) so a failed save can leave the user's attempted
  // text in place, with the error right below it, instead of silently
  // reverting mid-edit.
  const [editingValue, setEditingValue] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const mutation = useUpdateDisplayName()
  const inputRef = useRef<HTMLInputElement>(null)

  const fallback = getDisplayName(getAccessTokenEmail())
  const currentName = profile.display_name?.trim() || fallback.name

  function startEditing() {
    setError(null)
    setEditingValue(profile.display_name ?? '')
    // The input doesn't exist yet this render (it mounts once
    // editingValue stops being null) — focus it next frame instead of
    // fighting the not-yet-rendered element.
    requestAnimationFrame(() => inputRef.current?.focus())
  }

  function cancelEditing() {
    setEditingValue(null)
    setError(null)
  }

  async function save() {
    if (editingValue === null) return
    const validationError = validateDisplayName(editingValue)
    if (validationError) {
      setError(validationError)
      return
    }
    setError(null)
    try {
      await mutation.mutateAsync(editingValue.trim())
      setEditingValue(null)
    } catch (err) {
      setError(isApiError(err) ? errorMessage(err.message) : 'Could not save the name, please try again')
    }
  }

  function handleKeyDown(e: KeyboardEvent<HTMLInputElement>) {
    if (e.key === 'Enter') {
      e.preventDefault()
      save()
    } else if (e.key === 'Escape') {
      e.preventDefault()
      cancelEditing()
    }
  }

  if (editingValue === null) {
    return (
      <div className={styles.display}>
        <span className={styles.name}>{currentName}</span>
        <button type="button" className={styles.editButton} onClick={startEditing} aria-label="Edit name">
          <PencilIcon size={13} />
        </button>
      </div>
    )
  }

  return (
    <div className={styles.editRow}>
      <div className={styles.editField}>
        <Input
          ref={inputRef}
          value={editingValue}
          onChange={(e) => setEditingValue(e.target.value)}
          onKeyDown={handleKeyDown}
          error={Boolean(error)}
          aria-describedby={error ? 'display-name-error' : undefined}
          maxLength={80}
          placeholder={fallback.name}
        />
        {error && <ErrorText id="display-name-error">{error}</ErrorText>}
      </div>
      <div className={styles.editActions}>
        <Button type="button" className={styles.actionButton} loading={mutation.isPending} onClick={save}>
          Save
        </Button>
        <Button
          type="button"
          variant="secondary"
          className={styles.actionButton}
          disabled={mutation.isPending}
          onClick={cancelEditing}
        >
          Cancel
        </Button>
      </div>
    </div>
  )
}
