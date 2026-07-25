import type { HTMLAttributes } from 'react'
import styles from './Banner.module.css'

type BannerVariant = 'warning' | 'danger'

interface BannerProps extends HTMLAttributes<HTMLDivElement> {
  variant: BannerVariant
}

// A noticeable colored box, not small colored text — used for account-status
// alerts (frozen/closed) and for surfacing a failed balance fetch, both cases
// where a user should not have to squint to notice something's wrong.
export function Banner({ variant, className, ...props }: BannerProps) {
  const classes = [styles.banner, styles[variant], className].filter(Boolean).join(' ')
  return <div role="alert" className={classes} {...props} />
}
