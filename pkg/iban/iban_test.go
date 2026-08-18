package iban

import "testing"

// TestGenerate pins known-correct check digits for two hand-verified BBAN
// vectors (cross-checked against an independent mod-97 implementation, not
// just against this package's own Validate) — the check digits are exactly
// the part that's easy to get almost right.
func TestGenerate(t *testing.T) {
	tests := []struct {
		name          string
		bankCode      string
		sortCode      string
		accountNumber string
		want          string
	}{
		{"vector 1", "ZZZZ", "000042", "34567890", "IE34ZZZZ00004234567890"},
		{"vector 2", "ZZZZ", "000099", "00000001", "IE90ZZZZ00009900000001"},
		{"lower-case bank code is upper-cased", "zzzz", "000042", "34567890", "IE34ZZZZ00004234567890"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Generate(tt.bankCode, tt.sortCode, tt.accountNumber)
			if err != nil {
				t.Fatalf("Generate(%q, %q, %q) returned error: %v", tt.bankCode, tt.sortCode, tt.accountNumber, err)
			}
			if got != tt.want {
				t.Errorf("Generate(%q, %q, %q) = %q, want %q", tt.bankCode, tt.sortCode, tt.accountNumber, got, tt.want)
			}
			if err := Validate(got); err != nil {
				t.Errorf("Generate(%q, %q, %q) produced %q, which Validate rejects: %v", tt.bankCode, tt.sortCode, tt.accountNumber, got, err)
			}
		})
	}
}

// TestGenerate_InvalidParts guards the BBAN part-length/charset checks —
// wrong-shaped input must be rejected before it ever reaches the checksum
// math, not silently truncated or padded.
func TestGenerate_InvalidParts(t *testing.T) {
	tests := []struct {
		name          string
		bankCode      string
		sortCode      string
		accountNumber string
	}{
		{"bank code too short", "ZZZ", "000042", "34567890"},
		{"bank code has digits", "ZZ11", "000042", "34567890"},
		{"sort code too short", "ZZZZ", "00042", "34567890"},
		{"sort code has letters", "ZZZZ", "00004A", "34567890"},
		{"account number too short", "ZZZZ", "000042", "3456789"},
		{"account number has letters", "ZZZZ", "000042", "3456789A"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Generate(tt.bankCode, tt.sortCode, tt.accountNumber); err == nil {
				t.Errorf("Generate(%q, %q, %q) succeeded, want an error", tt.bankCode, tt.sortCode, tt.accountNumber)
			}
		})
	}
}

// TestValidate_Valid covers a real-world reference IBAN (proving the
// checksum math against an independently-known-correct example, not just
// against this package's own Generate), our own generated form, and
// human-entered formatting (spaces, lower case) that must normalize
// cleanly before the checksum check runs.
func TestValidate_Valid(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"real-world reference IBAN", "IE29AIBK93115212345678"},
		{"reference IBAN with spaces", "IE29 AIBK 9311 5212 3456 78"},
		{"reference IBAN lower case", "ie29aibk93115212345678"},
		{"reference IBAN lower case with spaces", "ie29 aibk 9311 5212 3456 78"},
		{"our own generated IBAN", "IE34ZZZZ00004234567890"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Validate(tt.in); err != nil {
				t.Errorf("Validate(%q) = %v, want nil", tt.in, err)
			}
		})
	}
}

// TestValidate_Invalid is the DoD's explicit requirement: a single
// corrupted digit must be rejected. mod-97-10 is designed to catch every
// single-character substitution error, so a naive "almost right" check-digit
// implementation is exactly what this test would catch.
func TestValidate_Invalid(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty string", ""},
		{"garbage", "not-an-iban"},
		{"one digit corrupted in the account number", "IE29AIBK93115212345679"},
		{"one digit corrupted in the check digits", "IE39AIBK93115212345678"},
		{"one letter corrupted in the bank code", "IE29AIBL93115212345678"},
		{"too short for IE", "IE29AIBK9311521234567"},
		{"too long for IE", "IE29AIBK931152123456789"},
		{"wrong checksum entirely", "IE00AIBK93115212345678"},
		{"non-alphanumeric character", "IE29AIBK9311521234567!"},
		{"lower-case checksum still fails when corrupted", "ie29aibk93115212345679"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Validate(tt.in); err == nil {
				t.Errorf("Validate(%q) = nil, want an error", tt.in)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"IE29 AIBK 9311 5212 3456 78", "IE29AIBK93115212345678"},
		{"ie29aibk93115212345678", "IE29AIBK93115212345678"},
		{"  ie29 aibk9311 5212345678 ", "IE29AIBK93115212345678"},
	}
	for _, tt := range tests {
		if got := Normalize(tt.in); got != tt.want {
			t.Errorf("Normalize(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormat(t *testing.T) {
	in := "IE29AIBK93115212345678"
	want := "IE29 AIBK 9311 5212 3456 78"
	if got := Format(in); got != want {
		t.Errorf("Format(%q) = %q, want %q", in, got, want)
	}
}

// TestFormat_NormalizesFirst ensures Format accepts the same messy input
// Validate does (spaces, lower case) rather than requiring pre-normalized
// input.
func TestFormat_NormalizesFirst(t *testing.T) {
	in := "ie29 aibk9311521234 5678"
	want := "IE29 AIBK 9311 5212 3456 78"
	if got := Format(in); got != want {
		t.Errorf("Format(%q) = %q, want %q", in, got, want)
	}
}
