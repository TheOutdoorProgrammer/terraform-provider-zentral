package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/zentralopensource/goztl"
)

func TestMDMEnrolledDeviceLookupPagination(t *testing.T) {
	const serial = "full-test-serial / 雪"
	for _, tc := range []struct {
		name    string
		second  []map[string]any
		wantID  int64
		wantErr bool
	}{
		{"active after history", []map[string]any{{"id": 3, "serial_number": serial, "platform": "iPadOS", "supervised": true, "blueprint": 2}}, 3, false},
		{"exact serial only", []map[string]any{{"id": 3, "serial_number": "other-prefix-雪"}}, 0, false},
		{"blocked", []map[string]any{{"id": 3, "serial_number": serial, "blocked_at": "blocked"}}, 0, false},
		{"ambiguous", []map[string]any{{"id": 3, "serial_number": serial}, {"id": 4, "serial_number": serial}}, 0, true},
		{"repeated page", []map[string]any{{"id": 1, "serial_number": serial}}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != "GET" || r.URL.Path != "/api/mdm/devices/" || r.URL.Query().Get("serial_number") != serial {
					t.Error("unsafe lookup request")
				}
				rows := []map[string]any{{"id": 1, "serial_number": serial, "checkout_at": "old"}, {"id": 2, "serial_number": serial, "blocked_at": "blocked"}}
				if r.URL.Query().Get("offset") == "2" {
					rows = tc.second
				} else if r.URL.Query().Get("offset") != "0" {
					t.Error("incorrect pagination offset")
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"count": 2 + len(tc.second), "results": rows, "next": "https://do-not-follow.invalid/private"})
			}))
			defer server.Close()
			client, err := goztl.NewClient(server.Client(), server.URL+"/api/", "fake-test-token")
			if err != nil {
				t.Fatal(err)
			}
			got, err := lookupEnrolledDevice(context.Background(), client, serial)
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected lookup result: %v", err)
			}
			if tc.wantID > 0 && (got == nil || got.ID != tc.wantID || got.BlueprintID == nil || *got.BlueprintID != 2) {
				t.Fatal("did not return exact active device")
			}
			if tc.wantID == 0 && got != nil {
				t.Fatal("unexpected device selected")
			}
			if requests != 2 {
				t.Fatalf("expected both pages, got %d requests", requests)
			}
		})
	}
}

func TestMDMEnrolledDeviceMissingAndErrors(t *testing.T) {
	for _, allow := range []bool{false, true} {
		for _, status := range []int{200, 403, 500} {
			t.Run(fmt.Sprintf("allow_%t_status_%d", allow, status), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != "GET" {
						t.Error("data source mutated server")
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					if status == 200 {
						_, _ = w.Write([]byte(`{"count":0,"results":[]}`))
					} else {
						_, _ = w.Write([]byte(`{"detail":"private-test-serial private-test-token"}`))
					}
				}))
				defer server.Close()
				client, err := goztl.NewClient(server.Client(), server.URL+"/api/", "fake-test-token")
				if err != nil {
					t.Fatal(err)
				}
				d := &MDMEnrolledDeviceDataSource{client: client}
				model := mdmEnrolledDeviceLookup{Serial: types.StringValue("private-test-serial"), AllowMissing: types.BoolValue(allow)}
				response := readLookupDataSource(t, d, &model)
				if response.Diagnostics.HasError() != (status != 200 || !allow) {
					t.Fatal(response.Diagnostics)
				}
				for _, diag := range response.Diagnostics {
					if strings.Contains(diag.Detail(), "private-test") {
						t.Fatal("private response reached diagnostic")
					}
				}
				if !response.Diagnostics.HasError() {
					var got mdmEnrolledDeviceLookup
					if diags := response.State.Get(context.Background(), &got); diags.HasError() {
						t.Fatal(diags)
					}
					if got.Found.ValueBool() || !got.ID.IsNull() || !got.Platform.IsNull() || !got.BlueprintID.IsNull() {
						t.Fatal("missing lookup retained computed values")
					}
				}
			})
		}
	}
}

func readLookupDataSource(t *testing.T, d datasource.DataSource, model any) datasource.ReadResponse {
	t.Helper()
	ctx := context.Background()
	var schemaResponse datasource.SchemaResponse
	d.Schema(ctx, datasource.SchemaRequest{}, &schemaResponse)
	state := tfsdk.State{Schema: schemaResponse.Schema}
	if diags := state.Set(ctx, model); diags.HasError() {
		t.Fatal(diags)
	}
	response := datasource.ReadResponse{State: tfsdk.State{Schema: schemaResponse.Schema}}
	d.Read(ctx, datasource.ReadRequest{Config: tfsdk.Config{Schema: schemaResponse.Schema, Raw: state.Raw}}, &response)
	return response
}

func TestMDMDeviceConfigurationLookup(t *testing.T) {
	for _, tc := range []struct {
		name, body     string
		status         int
		allow, wantErr bool
	}{
		{"attached", `{"found":true,"app_eligible":true,"artifact_id":"12345678-1234-4234-8234-000000000001","attachment_id":13,"tag_id":3}`, 200, false, false},
		{"detached", `{"found":true,"app_eligible":true,"artifact_id":"12345678-1234-4234-8234-000000000001","attachment_id":null,"tag_id":3}`, 200, false, false},
		{"pending", `{"found":false,"app_eligible":false,"artifact_id":null,"attachment_id":null,"tag_id":null}`, 200, true, false},
		{"malformed metadata", `{}`, 200, true, true},
		{"invalid artifact", `{"found":true,"app_eligible":true,"artifact_id":"invalid","tag_id":3}`, 200, true, true},
		{"app only", `{"found":false,"app_eligible":true,"artifact_id":null,"attachment_id":null,"tag_id":null}`, 200, true, false},
		{"missing required", `{"found":false,"app_eligible":false}`, 200, false, true},
		{"missing identity", `{"found":true,"artifact_id":null,"tag_id":3}`, 200, true, true},
		{"unexpected identity", `{"found":false,"artifact_id":"unexpected","tag_id":3}`, 200, true, true},
		{"permission", `{"detail":"private-test-secret"}`, 403, true, true},
		{"unsafe scope", `{"detail":"private-test-secret"}`, 400, true, true},
		{"server too old", `{"detail":"private-test-secret"}`, 404, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/mdm/devices/configuration_target/" || r.URL.Query().Get("serial_number") != "test-serial" || r.URL.Query().Get("app_name") != "VPN & App" {
					t.Error("unexpected configuration lookup request")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client, err := goztl.NewClient(server.Client(), server.URL+"/api/", "fake-test-token")
			if err != nil {
				t.Fatal(err)
			}
			d := &MDMDeviceConfigurationDataSource{client: client}
			model := mdmDeviceConfigurationLookup{Serial: types.StringValue("test-serial"), AppName: types.StringValue("VPN & App"), AllowMissing: types.BoolValue(tc.allow)}
			response := readLookupDataSource(t, d, &model)
			if response.Diagnostics.HasError() != tc.wantErr {
				t.Fatal(response.Diagnostics)
			}
			for _, diag := range response.Diagnostics {
				if strings.Contains(diag.Detail(), "private-test") {
					t.Fatal("private response reached diagnostic")
				}
			}
			if !tc.wantErr {
				var got mdmDeviceConfigurationLookup
				if diags := response.State.Get(context.Background(), &got); diags.HasError() {
					t.Fatal(diags)
				}
				if got.Found.ValueBool() != strings.Contains(tc.body, `"found":true`) {
					t.Fatal("wrong found result")
				}
				if got.AppEligible.ValueBool() != strings.Contains(tc.body, `"app_eligible":true`) {
					t.Fatal("wrong app eligibility result")
				}
			}
		})
	}
}

func TestMDMEnrolledDeviceDataSourceProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/mdm/devices/" {
			t.Error("unexpected data source request")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("serial_number") == "pending" {
			_, _ = w.Write([]byte(`{"count":0,"results":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"count":1,"results":[{"id":42,"serial_number":"exact","name":"Current name","platform":"iPadOS","supervised":true,"blueprint":2}]}`))
	}))
	defer server.Close()
	config := `provider "zentral" {
base_url = ` + strconv.Quote(server.URL+"/api/") + `
token="fake-test-token"
}
data "zentral_mdm_enrolled_device" "active" {serial_number="exact"}
`
	resource.UnitTest(t, resource.TestCase{ProtoV6ProviderFactories: testAccProtoV6ProviderFactories, Steps: []resource.TestStep{{Config: config, Check: resource.ComposeAggregateTestCheckFunc(
		resource.TestCheckResourceAttr("data.zentral_mdm_enrolled_device.active", "id", "42"),
		resource.TestCheckResourceAttr("data.zentral_mdm_enrolled_device.active", "blueprint_id", "2"),
		resource.TestCheckResourceAttr("data.zentral_mdm_enrolled_device.active", "found", "true"),
	)}}})
}
