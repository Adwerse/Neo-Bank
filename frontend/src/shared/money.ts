const CURRENCY_SYMBOLS: Record<string, string> = {
  EUR: '€',
}

// Renders an integer minor-units balance as a human string ("50000" ->
// "500.00 €"). Deliberately never treats the balance as a fractional
// number: Math.trunc(abs / 100) and abs % 100 are both exact for the
// integer inputs a balance actually is, unlike computing minorUnits / 100
// as a float and formatting that would be.
export function formatMoney(minorUnits: number, currency: string): string {
  const sign = minorUnits < 0 ? '-' : ''
  const abs = Math.abs(minorUnits)
  const whole = Math.trunc(abs / 100)
  const cents = abs % 100
  const symbol = CURRENCY_SYMBOLS[currency] ?? currency
  return `${sign}${groupThousands(whole)}.${String(cents).padStart(2, '0')} ${symbol}`
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
