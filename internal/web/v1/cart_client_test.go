package v1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCartClient_ClearCart(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := NewCartClient(srv.URL).ClearCart(context.Background(), "user-7"); err != nil {
		t.Fatalf("ClearCart err = %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/cart/v1/internal/cart/user-7" {
		t.Errorf("path = %q, want /cart/v1/internal/cart/user-7", gotPath)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty (no token on the internal call)", gotAuth)
	}
}

func TestCartClient_ClearCart_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if err := NewCartClient(srv.URL).ClearCart(context.Background(), "7"); err == nil {
		t.Fatal("ClearCart on non-2xx = nil, want an error")
	}
}

func TestCartClient_GetCart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer t" {
			t.Errorf("Authorization = %q, want 'Bearer t'", got)
		}
		_, _ = w.Write([]byte(`{"items":[{"product_id":"1","product_name":"x","product_price":1.5,"quantity":2}]}`))
	}))
	defer srv.Close()

	got, err := NewCartClient(srv.URL).GetCart(context.Background(), "Bearer t")
	if err != nil {
		t.Fatalf("GetCart err = %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].ProductID != "1" || got.Items[0].Quantity != 2 {
		t.Fatalf("cart = %+v", got)
	}
}

func TestCartClient_GetCart_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	if _, err := NewCartClient(srv.URL).GetCart(context.Background(), "Bearer t"); err == nil {
		t.Fatal("GetCart on non-2xx = nil, want an error")
	}
}
