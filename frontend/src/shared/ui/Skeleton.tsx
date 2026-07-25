import type { HTMLAttributes } from 'react'
import styles from './Skeleton.module.css'

type SkeletonProps = HTMLAttributes<HTMLDivElement>

export function Skeleton({ className, ...props }: SkeletonProps) {
  const classes = [styles.skeleton, className].filter(Boolean).join(' ')
  return <div className={classes} {...props} />
}
