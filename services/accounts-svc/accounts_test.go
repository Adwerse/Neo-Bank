package main

import "testing"

// TestIbanPartsFromAccountNumber_Injective pins the property the whole
// backfill/collision-free design in accounts.go depends on: distinct
// account numbers must never derive the same (sortCode, bbanAccountNumber)
// pair, since accounts.iban's uniqueness is not separately enforced by a
// retry loop the way accounts.account_number's is — it relies entirely on
// this mapping being injective.
func TestIbanPartsFromAccountNumber_Injective(t *testing.T) {
	accountNumbers := []string{
		"NB0000000000",
		"NB0000000001",
		"NB0000000010",
		"NB0000000100",
		"NB1234567890",
		"NB9999999999",
		"NB0417235968",
		"NB4200000001",
	}

	type parts struct{ sortCode, acctNum string }
	seen := make(map[parts]string)
	for _, an := range accountNumbers {
		sortCode, acctNum := ibanPartsFromAccountNumber(an)
		if len(sortCode) != 6 {
			t.Errorf("ibanPartsFromAccountNumber(%q) sortCode = %q, want length 6", an, sortCode)
		}
		if len(acctNum) != 8 {
			t.Errorf("ibanPartsFromAccountNumber(%q) bbanAccountNumber = %q, want length 8", an, acctNum)
		}
		p := parts{sortCode, acctNum}
		if prior, ok := seen[p]; ok {
			t.Errorf("ibanPartsFromAccountNumber(%q) and (%q) both produced %+v — not injective", an, prior, p)
		}
		seen[p] = an
	}
}

// TestIbanPartsFromAccountNumber_Exact pins the exact digit split so a
// future refactor can't silently change which digits land in the sort
// code vs. the account number field.
func TestIbanPartsFromAccountNumber_Exact(t *testing.T) {
	sortCode, acctNum := ibanPartsFromAccountNumber("NB0417235968")
	if want := "000004"; sortCode != want {
		t.Errorf("sortCode = %q, want %q", sortCode, want)
	}
	if want := "17235968"; acctNum != want {
		t.Errorf("bbanAccountNumber = %q, want %q", acctNum, want)
	}
}
