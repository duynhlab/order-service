package v1

import "github.com/duynhlab/order-service/internal/core/domain"

// isTestToken delegates to the single order-service copy of the token rule
// (domain.ValidPaymentToken) — shared with the gRPC transport since RFC-0015
// P2 so the two surfaces cannot drift.
func isTestToken(s string) bool {
	return domain.ValidPaymentToken(s)
}
