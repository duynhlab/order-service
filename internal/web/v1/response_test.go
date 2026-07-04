package v1

import (
	"testing"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// TestToOrderResponse_RendersDollars pins the HTTP contract: money is stored in
// minor units internally but the JSON response stays in dollars (non-breaking
// for the frontend). It also covers the item-mapping branch.
func TestToOrderResponse_RendersDollars(t *testing.T) {
	o := domain.Order{
		ID: "1", UserID: "u1", Status: "pending",
		Subtotal: 2050, Shipping: 500, Total: 2550,
		Items: []domain.OrderItem{
			{ProductID: "p1", ProductName: "Widget", Quantity: 2, Price: 1025, Subtotal: 2050},
		},
	}
	r := toOrderResponse(o)

	if r.Subtotal != 20.50 || r.Shipping != 5.00 || r.Total != 25.50 {
		t.Fatalf("order money must render as dollars, got subtotal=%v shipping=%v total=%v", r.Subtotal, r.Shipping, r.Total)
	}
	if len(r.Items) != 1 || r.Items[0].Price != 10.25 || r.Items[0].Subtotal != 20.50 {
		t.Fatalf("item money must render as dollars, got %+v", r.Items)
	}
	if r.ID != "1" || r.Status != "pending" || r.Items[0].ProductName != "Widget" {
		t.Fatalf("non-money fields must pass through unchanged, got %+v", r)
	}
}
