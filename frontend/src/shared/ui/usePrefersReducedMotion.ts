import { useSyncExternalStore } from 'react'

const QUERY = '(prefers-reduced-motion: reduce)'

function subscribe(callback: () => void): () => void {
  const mql = window.matchMedia(QUERY)
  mql.addEventListener('change', callback)
  return () => mql.removeEventListener('change', callback)
}

function getSnapshot(): boolean {
  return window.matchMedia(QUERY).matches
}

// The one place JS needs to know this — CSS handles its own
// `@media (prefers-reduced-motion: reduce)` guards independently; this is
// only for animation that a CSS media query can't reach (recharts' own
// JS-driven draw-in animation, say). Same subscribe/snapshot shape as
// useIsDesktop.
export function usePrefersReducedMotion(): boolean {
  return useSyncExternalStore(subscribe, getSnapshot, () => false)
}
