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
