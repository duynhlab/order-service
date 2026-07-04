package domain

import (
	"encoding/json"
	"testing"
)

// TestCreateOrderRequest_IgnoresClientItems guards the request-side contract:
// client-supplied items (including fractional dollar prices) must not break
// binding — items are server-sourced from the cart and never read from the body.
// Without json:"-" the int64 Price field would reject "price": 29.99 with a 400.
func TestCreateOrderRequest_IgnoresClientItems(t *testing.T) {
	var req CreateOrderRequest
	body := `{"items":[{"product_id":"1","quantity":2,"price":29.99}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("client body with a fractional item price must not fail binding: %v", err)
	}
	if len(req.Items) != 0 {
		t.Fatalf("client items must be ignored (server-sourced from cart), got %d", len(req.Items))
	}
}

func TestMinorUnits(t *testing.T) {
	cases := map[float64]int64{
		0:      0,
		5.00:   500,
		10.25:  1025,
		19.99:  1999,
		0.01:   1,
		1234.5: 123450,
	}
	for dollars, want := range cases {
		if got := MinorUnits(dollars); got != want {
			t.Errorf("MinorUnits(%.2f) = %d, want %d", dollars, got, want)
		}
	}
}

func TestDollars(t *testing.T) {
	cases := map[int64]float64{
		0:      0,
		500:    5.00,
		1025:   10.25,
		1999:   19.99,
		1:      0.01,
		123450: 1234.5,
	}
	for minor, want := range cases {
		if got := Dollars(minor); got != want {
			t.Errorf("Dollars(%d) = %.2f, want %.2f", minor, got, want)
		}
	}
}

// TestMoneyRoundTrip guards the property the boundary conversions rely on:
// a 2-decimal dollar value survives dollars → minor → dollars unchanged.
func TestMoneyRoundTrip(t *testing.T) {
	for _, d := range []float64{0, 5.00, 10.25, 19.99, 0.01, 999.99} {
		if got := Dollars(MinorUnits(d)); got != d {
			t.Errorf("round-trip %.2f -> %d -> %.2f", d, MinorUnits(d), got)
		}
	}
}
