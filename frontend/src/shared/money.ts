import { APP_LOCALE } from './locale'

// Intl.NumberFormat instances are expensive enough to construct that the
// spec explicitly documents caching them — one per (currency, display
// mode) pair, built lazily, never per-render.
const formatters = new Map<string, Intl.NumberFormat>()

function currencyFormatter(currency: string, currencyDisplay: 'symbol' | 'name'): Intl.NumberFormat {
  const key = `${currency}:${currencyDisplay}`
  let formatter = formatters.get(key)
  if (!formatter) {
    formatter = new Intl.NumberFormat(APP_LOCALE, { style: 'currency', currency, currencyDisplay })
    formatters.set(key, formatter)
  }
  return formatter
}

// State everywhere else in this app stays integer minor units (cents) —
// this is the one place that ever converts to a float, and only to hand
// it straight to Intl.NumberFormat for display. That's a different risk
// than parseAmountToCents' concern (features/transfers/money.ts): parsing
// user input with Math.round(parseFloat(x) * 100) can misround because
// the *multiplication* itself can land on the wrong side of an integer.
// Dividing a known-exact integer by 100 lands at most ~1e-13 off the true
// decimal value, and Intl.NumberFormat rounds to the currency's minor-unit
// precision from that value correctly — nowhere close to the 0.005
// threshold where such an error could flip the displayed cent.
export function formatMoney(minorUnits: number, currency: string): string {
  return currencyFormatter(currency, 'symbol').format(minorUnits / 100)
}

// Same number, currency spoken as a word ("500.00 euros") rather than a
// symbol — for aria-labels only, never rendered on screen. A screen
// reader's handling of a bare currency symbol varies (some skip "€"
// entirely, some say "Euro sign"), so accessible names use the word.
export function formatMoneySpoken(minorUnits: number, currency: string): string {
  return currencyFormatter(currency, 'name').format(minorUnits / 100)
}
