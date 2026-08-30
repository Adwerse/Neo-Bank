// Client-side mirror of pkg/iban/iban.go's Validate/Normalize/Format —
// same ISO 7064 mod-97-10 check-digit algorithm, kept in lockstep so the
// form can reject a malformed IBAN before it ever reaches the backend.
// Uses native BigInt instead of math/big; JS BigInt has no size limit, so
// the ~28-digit numeric form of an IE IBAN is exact here too.

export const COUNTRY_IE = 'IE'

const BANK_CODE_LENGTH = 4 // letters
const SORT_CODE_LENGTH = 6 // digits
const ACCOUNT_NUMBER_LENGTH = 8 // digits
export const IE_LENGTH = 2 + 2 + BANK_CODE_LENGTH + SORT_CODE_LENGTH + ACCOUNT_NUMBER_LENGTH // 22

const MOD97 = 97n

// Normalize returns the canonical storage form: no whitespace, upper case.
export function normalizeIban(s: string): string {
  return s.toUpperCase().replace(/\s+/g, '')
}

// Format groups the normalized form into 4-character blocks separated by
// spaces, for display — e.g. "IE29 AIBK 9311 5212 3456 78". Safe to call
// on partial input while the user is still typing.
export function formatIban(s: string): string {
  const n = normalizeIban(s)
  const blocks: string[] = []
  for (let i = 0; i < n.length; i += 4) {
    blocks.push(n.slice(i, i + 4))
  }
  return blocks.join(' ')
}

function isAllLetters(s: string): boolean {
  return s.length > 0 && /^[A-Z]+$/.test(s)
}

function isAllDigits(s: string): boolean {
  return s.length > 0 && /^[0-9]+$/.test(s)
}

function isAllLettersOrDigits(s: string): boolean {
  return /^[A-Z0-9]*$/.test(s)
}

// lettersToDigits implements ISO 7064's letter substitution: each letter
// A-Z becomes its two-digit position (A=10 ... Z=35), each digit passes
// through unchanged. Returns null on any other character.
function lettersToDigits(s: string): string | null {
  let out = ''
  for (const ch of s) {
    if (ch >= '0' && ch <= '9') {
      out += ch
    } else if (ch >= 'A' && ch <= 'Z') {
      out += String(ch.charCodeAt(0) - 'A'.charCodeAt(0) + 10)
    } else {
      return null
    }
  }
  return out
}

function mod97Remainder(s: string): bigint | null {
  const digits = lettersToDigits(s)
  if (digits === null) return null
  return BigInt(digits) % MOD97
}

// validateIban mirrors pkg/iban/iban.go's Validate: it reports why `s`
// isn't a valid IBAN, or null if it is. The mod-97 checksum applies to any
// country's IBAN; the fixed 4-letter/6-digit/8-digit field layout is only
// enforced for IE, matching the backend (the only country this package has
// an authoritative layout for).
export function validateIban(s: string): string | null {
  const n = normalizeIban(s)

  if (n.length < 4) return 'IBAN is too short'

  const country = n.slice(0, 2)
  const checkDigits = n.slice(2, 4)
  const bban = n.slice(4)

  if (!isAllLetters(country)) return 'Invalid country code'
  if (!isAllDigits(checkDigits)) return 'Invalid check digits'
  if (!isAllLettersOrDigits(bban)) return 'IBAN contains invalid characters'

  if (country === COUNTRY_IE) {
    if (n.length !== IE_LENGTH) return `An Irish IBAN must be ${IE_LENGTH} characters`
    const bankCode = bban.slice(0, BANK_CODE_LENGTH)
    const sortCode = bban.slice(BANK_CODE_LENGTH, BANK_CODE_LENGTH + SORT_CODE_LENGTH)
    const acctNum = bban.slice(BANK_CODE_LENGTH + SORT_CODE_LENGTH)
    if (!isAllLetters(bankCode)) return 'Invalid bank code'
    if (!isAllDigits(sortCode)) return 'Invalid sort code'
    if (!isAllDigits(acctNum)) return 'Invalid account number'
  }

  const remainder = mod97Remainder(bban + country + checkDigits)
  if (remainder === null) return 'IBAN contains invalid characters'
  if (remainder !== 1n) return 'Invalid IBAN check digits'

  return null
}

export function isValidIban(s: string): boolean {
  return validateIban(s) === null
}
