import { useEffect, useRef, useState } from 'react'

// A push update changes numbers on screen with no visible cause — in a
// banking app that reads as alarming rather than reassuring unless the UI
// itself explains "this just moved". Returns true for durationMs any time
// value differs from what it was on the previous render, false on the
// mount render itself (there's nothing to flash yet) and everywhere else.
export function useFlashOnChange<T>(value: T, durationMs = 1500): boolean {
  const prevRef = useRef(value)
  const [flashing, setFlashing] = useState(false)
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    if (prevRef.current !== value) {
      setFlashing(true)
      if (timerRef.current) clearTimeout(timerRef.current)
      timerRef.current = setTimeout(() => setFlashing(false), durationMs)
    }
    prevRef.current = value
    return () => {
      if (timerRef.current) clearTimeout(timerRef.current)
    }
  }, [value, durationMs])

  return flashing
}
