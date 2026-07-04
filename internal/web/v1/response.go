package v1

import (
	"time"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// orderItemResponse mirrors domain.OrderItem but renders money as a dollars
// float. Money is stored/computed in minor units; the HTTP contract stays in
// dollars, so the conversion happens here at the serialization boundary.
type orderItemResponse struct {
	ProductID   string  `json:"product_id"`
	ProductName string  `json:"product_name"`
	Quantity    int     `json:"quantity"`
	Price       float64 `json:"price"`
	Subtotal    float64 `json:"subtotal"`
}

// orderResponse mirrors domain.Order with money rendered as dollars.
type orderResponse struct {
	ID        string              `json:"id"`
	UserID    string              `json:"user_id"`
	Status    string              `json:"status"`
	Items     []orderItemResponse `json:"items"`
	Subtotal  float64             `json:"subtotal"`
	Shipping  float64             `json:"shipping"`
	Total     float64             `json:"total"`
	CreatedAt time.Time           `json:"created_at"`
}

// toOrderResponse maps a domain order (minor units) to its HTTP shape (dollars).
func toOrderResponse(o domain.Order) orderResponse {
	items := make([]orderItemResponse, len(o.Items))
	for i, it := range o.Items {
		items[i] = orderItemResponse{
			ProductID:   it.ProductID,
			ProductName: it.ProductName,
			Quantity:    it.Quantity,
			Price:       domain.Dollars(it.Price),
			Subtotal:    domain.Dollars(it.Subtotal),
		}
	}
	return orderResponse{
		ID:        o.ID,
		UserID:    o.UserID,
		Status:    o.Status,
		Items:     items,
		Subtotal:  domain.Dollars(o.Subtotal),
		Shipping:  domain.Dollars(o.Shipping),
		Total:     domain.Dollars(o.Total),
		CreatedAt: o.CreatedAt,
	}
}

// toOrderResponses maps a slice of domain orders to their HTTP shape.
func toOrderResponses(orders []domain.Order) []orderResponse {
	out := make([]orderResponse, len(orders))
	for i, o := range orders {
		out[i] = toOrderResponse(o)
	}
	return out
}
