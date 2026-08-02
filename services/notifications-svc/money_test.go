package main

import "testing"

// TestFormatMinorUnits pins the same cases frontend money.ts produces,
// minus the currency symbol. If these two ever disagree, a user sees one
// amount in the UI and a different-looking one in the email about it.
func TestFormatMinorUnits(t *testing.T) {
	tests := []struct {
		name       string
		minorUnits int64
		want       string
	}{
		{"zero", 0, "0.00"},
		{"cents only, one digit", 5, "0.05"},
		{"cents only, two digits", 99, "0.99"},
		{"exactly one unit", 100, "1.00"},
		{"no grouping needed", 99999, "999.99"},
		{"one separator", 123456, "1,234.56"},
		{"separator at a boundary", 100000, "1,000.00"},
		{"two separators", 100000000, "1,000,000.00"},
		{"negative", -2550, "-25.50"},
		{"negative with grouping", -123456789, "-1,234,567.89"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatMinorUnits(tt.minorUnits); got != tt.want {
				t.Errorf("formatMinorUnits(%d) = %q, want %q", tt.minorUnits, got, tt.want)
			}
		})
	}
}

// TestFormatMinorUnits_IsASCII guards the premise the email templates
// rest on. Every message this service sends omits a MIME charset header
// because its content is ASCII; a currency symbol sneaking into the
// formatter would break that in a customer's mail client rather than
// here.
func TestFormatMinorUnits_IsASCII(t *testing.T) {
	for _, minorUnits := range []int64{0, 5, 123456, -123456789, 100000000} {
		got := formatMinorUnits(minorUnits)
		if !isASCII(got) {
			t.Errorf("formatMinorUnits(%d) = %q, which is not ASCII — the templates send no charset header", minorUnits, got)
		}
	}
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}
