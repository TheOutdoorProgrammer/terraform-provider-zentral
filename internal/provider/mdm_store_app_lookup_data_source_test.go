package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/zentralopensource/goztl"
)

func storeAppLookupFixture() map[string]any {
	return map[string]any{
		"id": 42, "location": 7, "adam_id": "1441195209", "pricing_param": "STDQ",
		"name": "WireGuard", "bundle_id": "com.wireguard.ios", "product_type": "App", "device_assignable": true,
		"total_count": 5, "retired_count": 0, "assigned_count": 5, "available_count": 0,
	}
}

func storeAppLookupPage(results ...map[string]any) map[string]any {
	return map[string]any{"count": len(results), "next": nil, "results": results}
}

func storeAppLookupTestServer(t *testing.T, locations, assets map[string]any, status int) (*MDMStoreAppLookupDataSource, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			t.Error("app lookup attempted a mutation")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if req.Header.Get("Authorization") != "Token fake-test-token" || req.URL.Query().Get("limit") != "2" {
			t.Error("missing authentication or bounded page size")
		}
		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"detail":"private-response-body"}`))
			return
		}
		switch req.URL.Path {
		case "/api/mdm/locations/":
			if req.URL.Query().Get("name") != "Example & Family" {
				t.Error("location name not encoded exactly")
			}
			_ = json.NewEncoder(w).Encode(locations)
		case "/api/mdm/location_assets/":
			if req.URL.Query().Get("bundle_id") != "com.wireguard.ios" || req.URL.Query().Get("location_id") != "7" {
				t.Error("asset request did not filter exact location and bundle ID")
			}
			_ = json.NewEncoder(w).Encode(assets)
		default:
			t.Errorf("unexpected endpoint: %s", req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client, err := goztl.NewClient(server.Client(), server.URL+"/api/", "fake-test-token")
	if err != nil {
		t.Fatal(err)
	}
	return &MDMStoreAppLookupDataSource{client: client}, server
}

func TestMDMStoreAppLookup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(map[string]any, map[string]any, map[string]any)
		fail   bool
	}{
		{name: "all licenses assigned is valid"},
		{name: "missing location", fail: true, modify: func(l, a, item map[string]any) { l["count"], l["results"] = 0, []any{} }},
		{name: "ambiguous location", fail: true, modify: func(l, a, item map[string]any) { l["count"] = 2 }},
		{name: "invalid location identity", fail: true, modify: func(l, a, item map[string]any) {
			l["results"] = []any{map[string]any{"id": 7, "name": "Other location"}}
		}},
		{name: "missing asset", fail: true, modify: func(l, a, item map[string]any) { a["count"], a["results"] = 0, []any{} }},
		{name: "ambiguous pricing variants", fail: true, modify: func(l, a, item map[string]any) { a["count"] = 2 }},
		{name: "truncated results", fail: true, modify: func(l, a, item map[string]any) { a["results"] = []any{} }},
		{name: "missing count", fail: true, modify: func(l, a, item map[string]any) { delete(a, "count") }},
		{name: "untrusted next URL", fail: true, modify: func(l, a, item map[string]any) { a["next"] = "https://private-untrusted.invalid/" }},
		{name: "old API metadata", fail: true, modify: func(l, a, item map[string]any) { delete(item, "bundle_id") }},
		{name: "null metadata", fail: true, modify: func(l, a, item map[string]any) { item["name"] = nil }},
		{name: "wrong bundle", fail: true, modify: func(l, a, item map[string]any) { item["bundle_id"] = "com.example.other" }},
		{name: "wrong location", fail: true, modify: func(l, a, item map[string]any) { item["location"] = 8 }},
		{name: "book", fail: true, modify: func(l, a, item map[string]any) { item["product_type"] = "Book" }},
		{name: "not device assignable", fail: true, modify: func(l, a, item map[string]any) { item["device_assignable"] = false }},
		{name: "zero licenses", fail: true, modify: func(l, a, item map[string]any) { item["total_count"] = 0 }},
		{name: "fully retired", fail: true, modify: func(l, a, item map[string]any) { item["retired_count"] = 5 }},
		{name: "missing license counts", fail: true, modify: func(l, a, item map[string]any) { delete(item, "retired_count") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			locations := storeAppLookupPage(map[string]any{"id": 7, "name": "Example & Family"})
			item := storeAppLookupFixture()
			assets := storeAppLookupPage(item)
			if tc.modify != nil {
				tc.modify(locations, assets, item)
			}
			d, _ := storeAppLookupTestServer(t, locations, assets, 0)
			asset, err := d.lookup(context.Background(), "Example & Family", "com.wireguard.ios")
			if (err != nil) != tc.fail {
				t.Fatalf("unexpected lookup result: error=%v", err)
			}
			if err == nil && (asset.ID != 42 || asset.LocationID != 7 || asset.AdamID != "1441195209") {
				t.Fatalf("incorrect app identity: %#v", asset)
			}
		})
	}
}

func TestMDMStoreAppLookupAPIErrors(t *testing.T) {
	for _, status := range []int{401, 403, 404, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			d, _ := storeAppLookupTestServer(t, nil, nil, status)
			_, err := d.lookup(context.Background(), "Example & Family", "com.wireguard.ios")
			if err == nil || strings.Contains(err.Error(), "private-response-body") || strings.Contains(err.Error(), "Example & Family") {
				t.Fatalf("API failure was ignored or leaked response data: %v", err)
			}
		})
	}
}

func TestMDMStoreAppLookupDataSource(t *testing.T) {
	_, server := storeAppLookupTestServer(t,
		storeAppLookupPage(map[string]any{"id": 7, "name": "Example & Family"}),
		storeAppLookupPage(storeAppLookupFixture()), 0)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
provider "zentral" {
  base_url = %q
  token = "fake-test-token"
}
data "zentral_mdm_store_app_lookup" "test" {
  location_name = "Example & Family"
  bundle_id = "com.wireguard.ios"
}
`, server.URL+"/api/"),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("data.zentral_mdm_store_app_lookup.test", "id", "42"),
				resource.TestCheckResourceAttr("data.zentral_mdm_store_app_lookup.test", "location_id", "7"),
				resource.TestCheckResourceAttr("data.zentral_mdm_store_app_lookup.test", "adam_id", "1441195209"),
				resource.TestCheckResourceAttr("data.zentral_mdm_store_app_lookup.test", "pricing_param", "STDQ"),
				resource.TestCheckResourceAttr("data.zentral_mdm_store_app_lookup.test", "name", "WireGuard"),
			),
		}},
	})
}
