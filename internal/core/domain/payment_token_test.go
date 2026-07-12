package domain

import "testing"

func TestValidPaymentToken(t *testing.T) {
	cases := map[string]struct {
		token string
		want  bool
	}{
		"provider test token":     {"tok_visa_ok", true},
		"underscored":             {"tok_mc_declined_2", true},
		"missing prefix":          {"visa_ok", false},
		"empty":                   {"", false},
		"prefix only":             {"tok_", true},
		"too long":                {"tok_" + string(make([]byte, 61)), false},
		"raw PAN":                 {"4111111111111111", false},
		"PAN behind prefix":       {"tok_4111111111111111", false},
		"grouped PAN":             {"tok_4111_1111_1111_1111", false},
		"eleven digits ok":        {"tok_12345678901", true},
		"twelve digits rejected":  {"tok_123456789012", false},
		"illegal char":            {"tok_ok!", false},
		"space":                   {"tok_a b", false},
		"unicode":                 {"tok_ökay", false},
		"upper and lower letters": {"tok_ABCdef", true},
	}
	for name, tc := range cases {
		if got := ValidPaymentToken(tc.token); got != tc.want {
			t.Errorf("%s: ValidPaymentToken(%q) = %v, want %v", name, tc.token, got, tc.want)
		}
	}
}
