package v1

// isTestToken enforces the platform's PCI discipline on the checkout's
// payment_method before it rides the durable workflow input: a short opaque
// token (`tok_` + [A-Za-z0-9_], ≤ 64 chars) with no card-number-like digit
// run. This is defense-in-depth — the payment service validates identically
// and authoritatively at authorize (logicv1.IsTestToken there); this copy only
// keeps PAN-shaped strings out of Temporal history, so drift is fail-safe.
func isTestToken(s string) bool {
	if len(s) < 4 || s[:4] != "tok_" || len(s) > 64 {
		return false
	}
	// Count TOTAL digits, not the longest contiguous run: separators like `_`
	// must not let a grouped PAN ("tok_4111_1111_1111_1111") slip through.
	digits := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits++
			if digits >= 12 { // a card number is 13–19 digits
				return false
			}
		case (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_':
			// allowed
		default:
			return false
		}
	}
	return true
}
