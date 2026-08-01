package v1

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// CartClient handles HTTP calls to the cart service. Its only remaining call
// is the saga's best-effort cart-clear: order creation reads the cart via the
// checkout service since RFC-0015 P4, so nothing here fetches cart contents.
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

// ClearCart empties a user's cart via cart's internal, NetworkPolicy-fenced
// endpoint, identified by userID. No bearer token is sent (or carried in the
// Temporal workflow input that drives this call).
func (c *CartClient) ClearCart(ctx context.Context, userID string) error {
	endpoint := c.baseURL + "/cart/v1/internal/cart/" + url.PathEscape(userID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	// baseURL is a trusted in-cluster service address from config, not user input.
	resp, err := c.httpClient.Do(req) //nolint:gosec // G704: URL is config-sourced, not user-controlled
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
