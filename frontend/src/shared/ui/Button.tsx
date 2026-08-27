import { forwardRef } from 'react'
import type { ButtonHTMLAttributes } from 'react'
import { SpinnerIcon } from './icons'
import styles from './Button.module.css'

type ButtonVariant = 'primary' | 'secondary'

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant
  loading?: boolean
}

// forwardRef so a caller can move focus to a specific button (e.g. the
// primary action on a freshly-mounted confirmation step) via ref.current?.focus().
export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  { variant = 'primary', loading = false, disabled, className, children, ...props },
  ref,
) {
  const classes = [styles.button, styles[variant], className].filter(Boolean).join(' ')
  return (
    <button ref={ref} className={classes} disabled={disabled || loading} aria-busy={loading || undefined} {...props}>
      {loading && <SpinnerIcon size={14} className={styles.spinner} />}
      {children}
    </button>
  )
})
