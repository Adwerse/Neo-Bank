import { useEffect } from 'react'

const SUFFIX = ' — Neo-Bank'

// No router-level title/meta handling exists (a plain createBrowserRouter
// tree, no loaders) — this is the simplest thing that gives every route
// its own tab title without pulling in a routing-integrated head library.
// Restores the previous title on unmount so a route that renders
// conditionally (RequireAuth redirecting away, say) doesn't leave a stale
// title behind after it disappears.
export function useDocumentTitle(title: string): void {
  useEffect(() => {
    const previous = document.title
    document.title = `${title}${SUFFIX}`
    return () => {
      document.title = previous
    }
  }, [title])
}
