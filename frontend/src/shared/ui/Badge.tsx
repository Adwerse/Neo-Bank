import type { HTMLAttributes } from 'react'
import styles from './Badge.module.css'

type BadgeVariant = 'success' | 'warning' | 'danger' | 'pending'

interface BadgeProps extends HTMLAttributes<HTMLSpanElement> {
  variant: BadgeVariant
}

// Theme-aware status pill (completed/failed/pending/etc.) — separate from
// Tag, whose variants are deliberately theme-*invariant* (a fixed-contrast
// chip regardless of ground). Status colors need to track the theme.
export function Badge({ variant, className, ...props }: BadgeProps) {
  const classes = [styles.badge, styles[variant], className].filter(Boolean).join(' ')
  return <span className={classes} {...props} />
}
