import type { HTMLAttributes } from 'react'
import styles from './ErrorText.module.css'

type ErrorTextProps = HTMLAttributes<HTMLParagraphElement>

export function ErrorText({ className, ...props }: ErrorTextProps) {
  const classes = [styles.errorText, className].filter(Boolean).join(' ')
  return <p role="alert" className={classes} {...props} />
}
