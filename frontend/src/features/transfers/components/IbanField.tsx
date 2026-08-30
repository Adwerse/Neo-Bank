import type { ChangeEvent } from 'react'
import { useController, type Control } from 'react-hook-form'
import { Input } from '../../../shared/ui/Input'
import { ErrorText } from '../../../shared/ui/ErrorText'
import { CheckIcon } from '../../../shared/ui/icons'
import { formatIban, validateIban } from '../iban'
import type { TransferFormValues } from '../schemas'
import styles from './IbanField.module.css'

interface IbanFieldProps {
  control: Control<TransferFormValues>
}

// Formats as the user types (grouped into 4s, matching Format in
// pkg/iban/iban.go) and runs the same mod-97 check the backend does, so a
// malformed IBAN is caught before the request ever goes out — see
// ../iban.ts. Uses useController instead of plain register() because the
// formatting has to rewrite the field's value on every keystroke, which
// needs a controlled input.
export function IbanField({ control }: IbanFieldProps) {
  const {
    field: { value, onChange, onBlur, ref, name },
    fieldState: { error, isTouched },
  } = useController({ control, name: 'recipientIban', defaultValue: '' })

  function handleChange(e: ChangeEvent<HTMLInputElement>) {
    const input = e.target
    const raw = input.value
    const caret = input.selectionStart ?? raw.length
    const alnumBeforeCaret = raw.slice(0, caret).replace(/[^A-Za-z0-9]/g, '').length
    const formatted = formatIban(raw)

    onChange(formatted)

    // Keep the caret at the same *character* it was after, not the same
    // index — reformatting can insert/remove spaces ahead of the caret.
    requestAnimationFrame(() => {
      if (document.activeElement !== input) return
      let seen = 0
      let pos = formatted.length
      for (let i = 0; i < formatted.length; i++) {
        if (/[A-Za-z0-9]/.test(formatted[i])) {
          seen++
          if (seen === alnumBeforeCaret) {
            pos = i + 1
            break
          }
        }
      }
      if (alnumBeforeCaret === 0) pos = 0
      input.setSelectionRange(pos, pos)
    })
  }

  const trimmed = value.trim()
  const liveError = trimmed.length > 0 ? validateIban(trimmed) : null
  const showValid = trimmed.length > 0 && liveError === null
  // Only nag about an incomplete/invalid IBAN once the field has been
  // touched — react-hook-form's schema error (shown on submit attempt)
  // still fires regardless, this is just the live-typing hint on top.
  const showLiveError = isTouched && trimmed.length > 0 && liveError !== null && !error

  // Exactly one of these three is ever shown at a time, so a single
  // aria-describedby always points at whichever message is currently
  // rendered (or nothing, if the field is untouched and empty).
  const describedBy = error ? 'recipientIban-error' : showLiveError ? 'recipientIban-live-error' : showValid ? 'recipientIban-valid' : undefined

  return (
    <div className={styles.wrapper}>
      <Input
        id="recipientIban"
        name={name}
        ref={ref}
        autoComplete="off"
        spellCheck={false}
        inputMode="text"
        placeholder="IE29 AIBK 9311 5212 3456 78"
        autoFocus
        value={value}
        onChange={handleChange}
        onBlur={onBlur}
        error={Boolean(error) || showLiveError}
        aria-describedby={describedBy}
        className={showValid ? styles.inputValid : undefined}
      />
      {showValid && (
        <span id="recipientIban-valid" className={styles.validHint}>
          <CheckIcon size={13} /> Details look correct
        </span>
      )}
      {error && <ErrorText id="recipientIban-error">{error.message}</ErrorText>}
      {!error && showLiveError && <ErrorText id="recipientIban-live-error">{liveError}</ErrorText>}
    </div>
  )
}
