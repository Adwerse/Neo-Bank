// Parses a human-entered decimal amount ("100", "99.50", "99,50") into
// integer minor units (cents). Deliberately never Math.round(parseFloat(x) *
// 100) — floating point multiplication can misround (10.1 * 100 ===
// 1009.9999999999999 in JS). Splitting the string on the decimal separator
// and combining the whole/fractional parts as integers avoids that
// entirely. Returns null for anything that doesn't look like a plain
// non-negative decimal with at most 2 fractional digits — validation
// (positivity, format) is the caller's job via transferSchema; this just
// refuses to guess at malformed input.
export function parseAmountToCents(input: string): number | null {
  const match = /^(\d+)([.,](\d{1,2}))?$/.exec(input.trim())
  if (!match) return null

  const whole = Number(match[1])
  const fraction = match[3] ?? ''
  const cents = fraction.length === 1 ? Number(fraction) * 10 : Number(fraction || '0')
  return whole * 100 + cents
}
