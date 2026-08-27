import { describe, expect, it } from 'vitest'
import { formatIban, normalizeIban, validateIban } from './iban'

// IE29 AIBK 9311 5212 3456 78 is the standard published IE IBAN example
// (also used as the doc-comment example in pkg/iban/iban.go's Format) —
// a real mod-97-valid checksum, not a placeholder.
const VALID_IE_IBAN = 'IE29AIBK93115212345678'

describe('validateIban', () => {
  it('accepts a checksum-valid IE IBAN', () => {
    expect(validateIban(VALID_IE_IBAN)).toBeNull()
  })

  it('accepts the same IBAN with spaces and lowercase letters', () => {
    expect(validateIban('ie29 aibk 9311 5212 3456 78')).toBeNull()
  })

  it('rejects a wrong check digit', () => {
    expect(validateIban('IE28AIBK93115212345678')).not.toBeNull()
  })

  it('rejects a transposed BBAN digit (checksum catches it)', () => {
    expect(validateIban('IE29AIBK93115212345687')).not.toBeNull()
  })

  it('rejects the wrong length for an IE IBAN', () => {
    expect(validateIban('IE29AIBK9311521234567')).not.toBeNull()
  })

  it('rejects a non-alnum character', () => {
    expect(validateIban('IE29-AIBK93115212345678')).not.toBeNull()
  })

  it('rejects an empty or too-short string', () => {
    expect(validateIban('')).not.toBeNull()
    expect(validateIban('IE2')).not.toBeNull()
  })
})

describe('normalizeIban', () => {
  it('strips whitespace and upper-cases', () => {
    expect(normalizeIban('ie29 aibk 9311 5212 3456 78')).toBe(VALID_IE_IBAN)
  })
})

describe('formatIban', () => {
  it('groups into 4-character blocks', () => {
    expect(formatIban(VALID_IE_IBAN)).toBe('IE29 AIBK 9311 5212 3456 78')
  })

  it('formats partial input as typed', () => {
    expect(formatIban('ie29aibk93')).toBe('IE29 AIBK 93')
  })
})
