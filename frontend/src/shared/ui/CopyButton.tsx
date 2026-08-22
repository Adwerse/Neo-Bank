import { useState } from 'react'
import { CheckIcon, CopyIcon } from './icons'
import styles from './CopyButton.module.css'

const CONFIRM_MS = 1500

interface CopyButtonProps {
  value: string
  label?: string
}

export function CopyButton({ value, label = 'Скопировать' }: CopyButtonProps) {
  const [copied, setCopied] = useState(false)

  async function handleClick() {
    try {
      await navigator.clipboard.writeText(value)
      setCopied(true)
      setTimeout(() => setCopied(false), CONFIRM_MS)
    } catch {
      // Clipboard access can be denied (permissions, insecure context) — a
      // silent no-op is fine for a low-stakes, fully optional convenience.
    }
  }

  return (
    <button type="button" className={styles.button} onClick={handleClick} aria-label={label}>
      {copied ? <CheckIcon size={13} /> : <CopyIcon size={13} />}
      <span className={styles.confirm} role="status" aria-live="polite">
        {copied ? 'Скопировано' : ''}
      </span>
    </button>
  )
}
