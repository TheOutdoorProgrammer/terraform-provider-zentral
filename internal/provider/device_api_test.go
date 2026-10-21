package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zentralopensource/goztl"
)

func TestDeviceRequestSanitizesErrors(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"detail":"private-test-token private-test-serial private-test-name"}`))
			}))
			defer server.Close()
			client, err := goztl.NewClient(server.Client(), server.URL+"/", "private-test-token")
			if err != nil {
				t.Fatal(err)
			}
			gotStatus, err := deviceRequest(context.Background(), client, "POST", "private-test-serial/", map[string]string{"name": "private-test-name"}, nil)
			if err == nil || gotStatus != status {
				t.Fatalf("status=%d, error=%v", gotStatus, err)
			}
			if strings.Contains(err.Error(), "private-test") || strings.Contains(err.Error(), server.URL) {
				t.Fatalf("unsafe diagnostic: %v", err)
			}
		})
	}
}

func TestDeviceRequestCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { t.Error("canceled request reached server") }))
	defer server.Close()
	client, err := goztl.NewClient(server.Client(), server.URL+"/", "fake-test-token")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = deviceRequest(ctx, client, "GET", "devices/", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestDeviceRequestMalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"private-test-secret":`))
	}))
	defer server.Close()
	client, err := goztl.NewClient(server.Client(), server.URL+"/", "fake-test-token")
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	status, err := deviceRequest(context.Background(), client, "GET", "devices/", nil, &result)
	if err == nil || strings.Contains(err.Error(), "private-test") {
		t.Fatalf("unsafe or missing error: status=%d err=%v", status, err)
	}
}
