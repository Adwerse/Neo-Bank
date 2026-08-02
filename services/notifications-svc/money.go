package main

import "strconv"

// formatMinorUnits renders an integer minor-units amount the way the
// frontend does — 123456 -> "1,234.56" — so a user reading an email and
// the same transfer in the UI sees one number, not two spellings of it.
// Mirrors frontend/src/features/accounts/money.ts's formatMoney,
// including its central decision: never divide by 100 as a float.
// abs/100 and abs%100 are exact for the integers an amount actually is;
// formatting float64(minorUnits)/100 is not, and money is the last place
// to spend precision on convenience.
//
// One deliberate difference from the frontend: no currency symbol. The
// caller appends " EUR", because "€" is not ASCII and every email this
// service sends is ASCII-only — which is what lets the templates omit a
// MIME charset header, matching auth-svc's plain-text messages. There is
// no currency field anywhere in this system to select a symbol with
// anyway; EUR is implicit end to end.
//
// math.MinInt64 would overflow on negation and is left unguarded (the
// frontend has the identical latent case): amounts reaching this service
// come from transfers-svc, which rejects anything <= 0 as invalid_amount
// long before an event is published.
func formatMinorUnits(minorUnits int64) string {
	sign := ""
	abs := minorUnits
	if abs < 0 {
		sign = "-"
		abs = -abs
	}
	whole := abs / 100
	cents := abs % 100

	centsStr := strconv.FormatInt(cents, 10)
	if len(centsStr) == 1 {
		centsStr = "0" + centsStr
	}
	return sign + groupThousands(whole) + "." + centsStr
}

// groupThousands inserts "," every three digits from the right.
func groupThousands(whole int64) string {
	digits := strconv.FormatInt(whole, 10)
	var b []byte
	for i := 0; i < len(digits); i++ {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b = append(b, ',')
		}
		b = append(b, digits[i])
	}
	return string(b)
}
