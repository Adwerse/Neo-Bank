// The one locale this app ever formats in. Hardcoded rather than read
// from the browser (navigator.language) — the interface has exactly one
// language (English) and no switcher, so formatting off the visitor's
// own locale would desync money/date punctuation from the copy around it
// for anyone whose browser isn't already set to English. "en-IE" specifically
// (not plain "en" or "en-US") because the fictional bank's own IBANs are
// Irish — its number/date conventions match en-US/en-GB anyway.
export const APP_LOCALE = 'en-IE'

const pluralRules = new Intl.PluralRules(APP_LOCALE)

// English only has two plural forms, but this still goes through
// Intl.PluralRules rather than a hand-rolled `count === 1` check — the
// rule, not a bespoke conditional, is what decides which form applies,
// consistent with every other pluralization in this app (relativeTime.ts,
// Intl.NumberFormat's unit style) never hand-rolling that decision.
export function pluralize(count: number, singular: string, plural: string): string {
  return pluralRules.select(count) === 'one' ? singular : plural
}
