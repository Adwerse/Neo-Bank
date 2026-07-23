import { forwardRef } from 'react'
import type { InputHTMLAttributes } from 'react'
import styles from './Input.module.css'

type InputProps = InputHTMLAttributes<HTMLInputElement>

// forwardRef so this can be passed straight to react-hook-form's register()
// once the auth forms are built — form wiring is deliberately not part of
// this scaffolding pass, but the primitive needs to already support it.
export const Input = forwardRef<HTMLInputElement, InputProps>(function Input(
  { className, ...props },
  ref,
) {
  const classes = [styles.input, className].filter(Boolean).join(' ')
  return <input ref={ref} className={classes} {...props} />
})
