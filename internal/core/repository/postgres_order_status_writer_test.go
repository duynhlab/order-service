package repository

import (
	"strings"
	"testing"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// updateForTransition is a pure function; its switch is pinned here so every
// destination's SQL carries the right metadata columns and the two
// unreachable arms stay loud.
func TestUpdateForTransition(t *testing.T) {
	wantFragments := map[domain.OrderStatus][]string{
		domain.OrderStatusConfirmed:    {"confirmed_at = NOW()", "workflow_id", "run_id"},
		domain.OrderStatusCompleted:    {"completed_at = NOW()"},
		domain.OrderStatusFailed:       {"failure_code = NULLIF($5, '')"},
		domain.OrderStatusManualReview: {"manual_review_reason = NULLIF($5, '')"},
		domain.OrderStatusCancelling:   {"cancellation_reason = NULLIF($5, '')"},
		domain.OrderStatusCancelled:    {"cancelled_at = NOW()"},
	}
	for to, fragments := range wantFragments {
		q := updateForTransition(to)
		for _, want := range []string{
			"version = version + 1",
			"updated_at = NOW()", // the reconciler's scan window depends on it
			"WHERE id = $1 AND status = $2 AND version = $3",
		} {
			if !strings.Contains(q, want) {
				t.Errorf("%s: query missing %q", to, want)
			}
		}
		for _, want := range fragments {
			if !strings.Contains(q, want) {
				t.Errorf("%s: query missing %q", to, want)
			}
		}
	}

	for name, to := range map[string]domain.OrderStatus{
		"pending is never a destination": domain.OrderStatusPending,
		"unknown status":                 "shipped",
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: expected panic", name)
				}
			}()
			updateForTransition(to)
		}()
	}
}
