import { useSyncExternalStore } from 'react'

const QUERY = '(min-width: 1024px)'

function subscribe(callback: () => void): () => void {
  const mql = window.matchMedia(QUERY)
  mql.addEventListener('change', callback)
  return () => mql.removeEventListener('change', callback)
}

function getSnapshot(): boolean {
  return window.matchMedia(QUERY).matches
}

// Single source of truth for the mobile/desktop split: Layout.tsx uses it to
// pick the top-bar shell vs. the sidebar DesktopShell, and DashboardPage.tsx
// uses the same hook to pick the Terminal vs. Blueprint content — so shell
// and content can never disagree about which "mode" is active. Binary
// switch, no intermediate tablet layout.
export function useIsDesktop(): boolean {
  return useSyncExternalStore(subscribe, getSnapshot, () => false)
}
