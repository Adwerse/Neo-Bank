// Client-side mirror of services/auth-svc/profile.go's validateDisplayName
// — same limit and character rules, so a bad name is caught before the
// PATCH round-trip instead of only after. The server remains the
// authoritative check; this is purely an instant-feedback hint.
export const MAX_DISPLAY_NAME_LENGTH = 50

// C0/C1 control characters (matches Go's unicode.IsControl for the BMP
// range every realistic display name lives in) plus the Unicode bidi
// embedding/override/isolate controls the backend also rejects — those
// can make a name render as something visually different from what it
// actually contains.
function isProblemChar(codePoint: number): boolean {
  if (codePoint <= 0x1f || (codePoint >= 0x7f && codePoint <= 0x9f)) return true
  if (codePoint >= 0x202a && codePoint <= 0x202e) return true
  if (codePoint >= 0x2066 && codePoint <= 0x2069) return true
  return false
}

// Returns an error message, or null if `value` is valid — including the
// empty/whitespace-only case, which is valid (it clears the name).
export function validateDisplayName(value: string): string | null {
  const trimmed = value.trim()
  if (trimmed.length === 0) return null

  const codePoints = Array.from(trimmed)
  if (codePoints.length > MAX_DISPLAY_NAME_LENGTH) {
    return `Имя должно быть не длиннее ${MAX_DISPLAY_NAME_LENGTH} символов`
  }
  for (const ch of codePoints) {
    const codePoint = ch.codePointAt(0)
    if (codePoint !== undefined && isProblemChar(codePoint)) {
      return 'Имя содержит недопустимые символы'
    }
  }
  return null
}
