// No "name" field exists anywhere in this API or the JWT — accounts and
// auth only ever carry an email. This derives a display name from the
// email's local part rather than inventing one.
const FALLBACK_NAME = 'Client'

export interface DisplayName {
  name: string
  initial: string
}

export function getDisplayName(email: string | null | undefined): DisplayName {
  const local = email?.split('@')[0] ?? ''
  const cleaned = local.replace(/[._+-]+/g, ' ').trim()
  if (!cleaned) return { name: FALLBACK_NAME, initial: FALLBACK_NAME[0] }

  const name = cleaned
    .split(' ')
    .filter(Boolean)
    .map((word) => word[0].toUpperCase() + word.slice(1))
    .join(' ')

  return { name, initial: name[0].toUpperCase() }
}
