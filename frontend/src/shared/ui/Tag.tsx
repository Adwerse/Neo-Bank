import type { HTMLAttributes } from 'react'
import styles from './Tag.module.css'

type TagVariant = 'accent' | 'neutral' | 'outline'

interface TagProps extends HTMLAttributes<HTMLSpanElement> {
  variant?: TagVariant
}

export function Tag({ variant = 'neutral', className, ...props }: TagProps) {
  const classes = [styles.tag, styles[variant], className].filter(Boolean).join(' ')
  return <span className={classes} {...props} />
}
