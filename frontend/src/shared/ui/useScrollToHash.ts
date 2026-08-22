import { useEffect } from 'react'
import { useLocation } from 'react-router'

// React Router's client-side navigation does not auto-scroll to a `#hash`
// target the way a full page load does — without this, a link like
// `/dashboard#account-details` would silently do nothing.
export function useScrollToHash(): void {
  const location = useLocation()

  useEffect(() => {
    if (!location.hash) return
    const id = location.hash.slice(1)

    const existing = document.getElementById(id)
    if (existing) {
      existing.scrollIntoView({ behavior: 'smooth', block: 'start' })
      return
    }

    // The target's data (useMe/useDashboardOperations) may still be loading
    // on the render where this first sees the hash, so the element isn't
    // mounted yet — watch for it instead of guessing a timeout.
    const observer = new MutationObserver(() => {
      const found = document.getElementById(id)
      if (found) {
        found.scrollIntoView({ behavior: 'smooth', block: 'start' })
        observer.disconnect()
      }
    })
    observer.observe(document.body, { childList: true, subtree: true })
    return () => observer.disconnect()
  }, [location.hash])
}
