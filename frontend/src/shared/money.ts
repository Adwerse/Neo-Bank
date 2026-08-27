const CURRENCY_SYMBOLS: Record<string, string> = {
  EUR: '€',
}

// Spoken form for aria-labels — a screen reader's handling of a bare
// currency symbol varies (some skip "€" entirely, some say "Euro sign"),
// so accessible names use the word instead of the glyph.
const CURRENCY_NAMES: Record<string, string> = {
  EUR: 'евро',
}

// Deliberately never treats the balance as a fractional number:
// Math.trunc(abs / 100) and abs % 100 are both exact for the integer
// inputs a balance actually is, unlike computing minorUnits / 100 as a
// float and formatting that would be.
function formatAmount(minorUnits: number): string {
  const sign = minorUnits < 0 ? '-' : ''
  const abs = Math.abs(minorUnits)
  const whole = Math.trunc(abs / 100)
  const cents = abs % 100
  return `${sign}${groupThousands(whole)}.${String(cents).padStart(2, '0')}`
}

// Renders an integer minor-units balance as a human string ("50000" ->
// "500.00 €"), for on-screen display.
export function formatMoney(minorUnits: number, currency: string): string {
  const symbol = CURRENCY_SYMBOLS[currency] ?? currency
  return `${formatAmount(minorUnits)} ${symbol}`
}

// Same number, currency spoken as a word ("500.00 евро") — for aria-labels
// only, never rendered on screen.
export function formatMoneySpoken(minorUnits: number, currency: string): string {
  const name = CURRENCY_NAMES[currency] ?? currency
  return `${formatAmount(minorUnits)} ${name}`
}

function groupThousands(whole: number): string {
  const digits = String(whole)
  let result = ''
  for (let i = 0; i < digits.length; i++) {
    if (i > 0 && (digits.length - i) % 3 === 0) {
      result += ','
    }
    result += digits[i]
  }
  return result
}
