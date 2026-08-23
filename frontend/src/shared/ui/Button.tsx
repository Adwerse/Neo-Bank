import type { ButtonHTMLAttributes } from 'react'
import { SpinnerIcon } from './icons'
import styles from './Button.module.css'

type ButtonVariant = 'primary' | 'secondary'

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant
  loading?: boolean
}

export function Button({ variant = 'primary', loading = false, disabled, className, children, ...props }: ButtonProps) {
  const classes = [styles.button, styles[variant], className].filter(Boolean).join(' ')
  return (
    <button className={classes} disabled={disabled || loading} aria-busy={loading || undefined} {...props}>
      {loading && <SpinnerIcon size={14} className={styles.spinner} />}
      {children}
    </button>
  )
}
