// Package iban generates and validates Irish (IE) IBANs using the ISO
// 7064 mod-97-10 check-digit algorithm. It knows nothing about any
// particular bank's account-numbering scheme — callers supply the BBAN
// parts (bank code, sort code, account number) and get back a
// checksum-valid IBAN, or validate an arbitrary input string.
package iban

import (
	"fmt"
	"math/big"
	"strings"
)

// CountryIE is the ISO 3166-1 alpha-2 country code this package generates
// IBANs for. Validate also accepts other countries' IBANs (the mod-97
// checksum is country-agnostic per ISO 7064) but only enforces IE's fixed
// field layout.
const CountryIE = "IE"

// IELength is the fixed length of a normalized IE IBAN. Exported because
// Validate only enforces this shape for country code "IE" — for any other
// country it just checks the generic checksum, with no minimum length
// beyond 4 — so a caller that needs to safely slice into the fixed IE BBAN
// layout (bank code, sort code, account number) must check both the
// country code AND this length first, not assume Validate already implies
// it for arbitrary input.
const IELength = 2 + 2 + bankCodeLength + sortCodeLength + bbanAccountNumberLength // 22

const (
	bankCodeLength          = 4 // letters
	sortCodeLength          = 6 // digits
	bbanAccountNumberLength = 8 // digits
	mod97Modulus            = 97
)

var mod97 = big.NewInt(mod97Modulus)

// Generate builds a checksum-valid IE IBAN from BBAN parts: a 4-letter
// bank/institution code, a 6-digit sort code, and an 8-digit account
// number. bankCode is upper-cased before use; sortCode and accountNumber
// must already be digit strings of the exact required length.
func Generate(bankCode, sortCode, accountNumber string) (string, error) {
	bankCode = strings.ToUpper(bankCode)
	if len(bankCode) != bankCodeLength || !isAllLetters(bankCode) {
		return "", fmt.Errorf("iban: bank code must be %d letters, got %q", bankCodeLength, bankCode)
	}
	if len(sortCode) != sortCodeLength || !isAllDigits(sortCode) {
		return "", fmt.Errorf("iban: sort code must be %d digits, got %q", sortCodeLength, sortCode)
	}
	if len(accountNumber) != bbanAccountNumberLength || !isAllDigits(accountNumber) {
		return "", fmt.Errorf("iban: account number must be %d digits, got %q", bbanAccountNumberLength, accountNumber)
	}

	bban := bankCode + sortCode + accountNumber

	// ISO 7064 mod-97-10: compute the check digits by first treating them
	// as "00", i.e. rearranging BBAN + country + "00" (the "move the first
	// four characters to the end" step applied to a not-yet-existing IBAN
	// whose check digits are provisionally zero).
	remainder, err := mod97Remainder(bban + CountryIE + "00")
	if err != nil {
		return "", err
	}
	checkDigits := 98 - remainder
	return fmt.Sprintf("%s%02d%s", CountryIE, checkDigits, bban), nil
}

// Validate normalizes s (strips whitespace, upper-cases — see Normalize)
// and reports whether it is a structurally valid, checksum-correct IBAN.
// The mod-97 checksum check applies to any country's IBAN; the fixed
// 4-letter/6-digit/8-digit field layout is only enforced for IE, since
// that's the only country format this package has authoritative lengths
// for.
func Validate(s string) error {
	n := Normalize(s)

	if len(n) < 4 {
		return fmt.Errorf("iban: %q is too short to be an IBAN", s)
	}
	country := n[0:2]
	checkDigits := n[2:4]
	bban := n[4:]

	if !isAllLetters(country) {
		return fmt.Errorf("iban: %q has an invalid country code %q", s, country)
	}
	if !isAllDigits(checkDigits) {
		return fmt.Errorf("iban: %q has non-numeric check digits %q", s, checkDigits)
	}
	if !isAllLettersOrDigits(bban) {
		return fmt.Errorf("iban: %q contains characters other than letters and digits", s)
	}

	if country == CountryIE {
		if len(n) != IELength {
			return fmt.Errorf("iban: IE IBANs must be %d characters, %q is %d", IELength, s, len(n))
		}
		bankCode := bban[0:bankCodeLength]
		sortCode := bban[bankCodeLength : bankCodeLength+sortCodeLength]
		acctNum := bban[bankCodeLength+sortCodeLength:]
		if !isAllLetters(bankCode) {
			return fmt.Errorf("iban: %q has an invalid bank code %q", s, bankCode)
		}
		if !isAllDigits(sortCode) {
			return fmt.Errorf("iban: %q has an invalid sort code %q", s, sortCode)
		}
		if !isAllDigits(acctNum) {
			return fmt.Errorf("iban: %q has an invalid account number %q", s, acctNum)
		}
	}

	// Validation rearranges the SAME way generation does, but with the
	// IBAN's actual check digits rather than a "00" placeholder: a
	// correct IBAN's rearranged numeric form is congruent to 1 mod 97.
	remainder, err := mod97Remainder(bban + country + checkDigits)
	if err != nil {
		return fmt.Errorf("iban: %q: %w", s, err)
	}
	if remainder != 1 {
		return fmt.Errorf("iban: %q fails the mod-97 checksum", s)
	}
	return nil
}

// Normalize returns the canonical storage form: no whitespace, upper case.
func Normalize(s string) string {
	return strings.ToUpper(strings.ReplaceAll(s, " ", ""))
}

// Format groups the normalized form into 4-character blocks separated by
// spaces, for display — e.g. "IE29 AIBK 9311 5212 3456 78".
func Format(s string) string {
	n := Normalize(s)
	var b strings.Builder
	for i := 0; i < len(n); i += 4 {
		if i > 0 {
			b.WriteByte(' ')
		}
		end := i + 4
		if end > len(n) {
			end = len(n)
		}
		b.WriteString(n[i:end])
	}
	return b.String()
}

// mod97Remainder converts s (a rearranged IBAN candidate: BBAN + country +
// check digits, in that order) to its ISO 7064 numeric form — letters
// become two-digit numbers (A=10 .. Z=35), digits pass through — and
// returns that (very large) number mod 97. math/big is required: a 22-char
// IE IBAN's numeric form runs to ~28 digits, well past int64 range.
func mod97Remainder(s string) (int64, error) {
	digits, err := lettersToDigits(s)
	if err != nil {
		return 0, err
	}
	n, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return 0, fmt.Errorf("iban: could not parse %q as a number", digits)
	}
	return new(big.Int).Mod(n, mod97).Int64(), nil
}

// lettersToDigits implements ISO 7064's letter substitution: each letter
// A-Z becomes its two-digit position (A=10 ... Z=35), each digit passes
// through unchanged.
func lettersToDigits(s string) (string, error) {
	var b strings.Builder
	b.Grow(len(s) * 2)
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			fmt.Fprintf(&b, "%d", r-'A'+10)
		default:
			return "", fmt.Errorf("iban: invalid character %q", r)
		}
	}
	return b.String(), nil
}

func isAllLetters(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isAllLettersOrDigits(s string) bool {
	for _, r := range s {
		isLetter := r >= 'A' && r <= 'Z'
		isDigit := r >= '0' && r <= '9'
		if !isLetter && !isDigit {
			return false
		}
	}
	return true
}
