package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// CartItem is the subset of a cart-service cart item the order service needs to
// build an order. ProductPrice is the authoritative, server-side price.
type CartItem struct {
	ProductID    string  `json:"product_id"`
	ProductName  string  `json:"product_name"`
	ProductPrice float64 `json:"product_price"`
	Quantity     int     `json:"quantity"`
}

// CartResponse is the subset of cart-service's GET /cart/v1/private/cart payload.
type CartResponse struct {
	Items []CartItem `json:"items"`
}

// CartClient handles HTTP calls to the cart service
type CartClient struct {
	baseURL    string
	httpClient *http.Client
}

func NewCartClient(baseURL string) *CartClient {
	return &CartClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 3 * time.Second,
		},
	}
}

// ClearCart clears the authenticated user's cart by calling cart service.
// It forwards the original Authorization header to preserve identity.
func (c *CartClient) ClearCart(ctx context.Context, authHeader string) error {
	// Cart private endpoint — forwards caller's Authorization header for JWT validation.
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/cart/v1/private/cart", nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request cart service: %w", err)
	}
	defer resp.Body.Close()

	// Treat any non-2xx as error (best-effort caller decides what to do)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("cart service returned status %d", resp.StatusCode)
	}
	return nil
}

// GetCart fetches the authenticated user's cart. This is the server-side source
// of truth for order pricing — the order service never trusts client-supplied
// prices. It forwards the caller's Authorization header for JWT validation.
func (c *CartClient) GetCart(ctx context.Context, authHeader string) (*CartResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/cart/v1/private/cart", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request cart service: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cart service returned status %d", resp.StatusCode)
	}

	var cart CartResponse
	if err := json.NewDecoder(resp.Body).Decode(&cart); err != nil {
		return nil, fmt.Errorf("decode cart response: %w", err)
	}
	return &cart, nil
}

// Global cart client (set during init)
var cartClient *CartClient

func SetCartClient(client *CartClient) {
	cartClient = client
}

func getCartClient(logger *zap.Logger) *CartClient {
	if cartClient == nil && logger != nil {
		logger.Warn("Cart client not initialized")
	}
	return cartClient
}

