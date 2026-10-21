package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/zentralopensource/goztl"
)

type machineTagTestServer struct {
	mu         sync.Mutex
	server     *httptest.Server
	serial     string
	tags       map[int]string
	membership map[int]bool
	posts      []machineTagRequest
	getStatus  int
	postStatus int
}

func newMachineTagTestServer(t *testing.T) *machineTagTestServer {
	t.Helper()
	f := &machineTagTestServer{
		serial:     ".example/device 雪",
		tags:       map[int]string{17: "managed", 18: "unrelated", 19: "replacement"},
		membership: map[int]bool{18: true},
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if req.Header.Get("Authorization") != "Token fake-test-token" {
			t.Error("missing test authentication")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		machinePath := "/api/inventory/machines/." + base64.RawURLEncoding.EncodeToString([]byte(f.serial)) + "/meta/"
		switch {
		case req.URL.Path == machinePath && req.Method == http.MethodGet:
			if f.getStatus != 0 {
				w.WriteHeader(f.getStatus)
				_, _ = w.Write([]byte(`{"detail":"private-test-body"}`))
				return
			}
			tags := []goztl.Tag{}
			for id := range f.membership {
				tags = append(tags, goztl.Tag{ID: id, Name: f.tags[id]})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"serial_number": f.serial, "tags": tags})
		case strings.HasPrefix(req.URL.Path, "/api/inventory/tags/") && req.Method == http.MethodGet:
			id, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/api/inventory/tags/"), "/"))
			name, ok := f.tags[id]
			if err != nil || !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"detail":"private-test-body"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(goztl.Tag{ID: id, Name: name})
		case req.URL.Path == "/api/inventory/machines/tags/" && req.Method == http.MethodPost:
			var body machineTagRequest
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.posts = append(f.posts, body)
			if f.postStatus != 0 {
				w.WriteHeader(f.postStatus)
				_, _ = w.Write([]byte(`{"detail":"private-test-body"}`))
				return
			}
			if !reflect.DeepEqual(body.SerialNumbers, []string{f.serial}) || len(body.Operations) != 1 || len(body.Operations[0].Names) != 1 {
				t.Error("mutation targeted more than the exact association")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			operation := body.Operations[0]
			for id, name := range f.tags {
				if name != operation.Names[0] {
					continue
				}
				switch operation.Kind {
				case "ADD":
					f.membership[id] = true
				case "REMOVE":
					delete(f.membership, id)
				default:
					t.Errorf("destructive operation %q", operation.Kind)
				}
			}
			_, _ = w.Write([]byte(`{"machines":{"found":1},"tags":{"added":1,"removed":0}}`))
		default:
			t.Errorf("unexpected endpoint: %s %s", req.Method, req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *machineTagTestServer) resource(t *testing.T) *MachineTagResource {
	t.Helper()
	client, err := goztl.NewClient(f.server.Client(), f.server.URL+"/api/", "fake-test-token")
	if err != nil {
		t.Fatal(err)
	}
	return &MachineTagResource{client: client}
}

func (f *machineTagTestServer) config(tagID int) string {
	return fmt.Sprintf(`
provider "zentral" {
  base_url = %q
  token = "fake-test-token"
}
resource "zentral_machine_tag" "test" {
  serial_number = %q
  tag_id = %d
}
`, f.server.URL+"/api/", f.serial, tagID)
}

func TestMachineTagResourceLifecycle(t *testing.T) {
	f := newMachineTagTestServer(t)
	name := "zentral_machine_tag.test"
	check := func(tagID, postCount int) resource.TestCheckFunc {
		return func(_ *terraform.State) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if !f.membership[18] || !f.membership[tagID] || len(f.posts) != postCount {
				return fmt.Errorf("unexpected membership or mutation count: %v, %d", f.membership, len(f.posts))
			}
			return nil
		}
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: func(_ *terraform.State) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if !reflect.DeepEqual(f.membership, map[int]bool{18: true}) {
				return fmt.Errorf("destroy changed unrelated tags or retained owned membership: %v", f.membership)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: f.config(17),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(name, "id", machineTagID(f.serial, 17)),
					resource.TestCheckResourceAttr(name, "serial_number", f.serial),
					check(17, 1),
				),
			},
			{Config: f.config(17), PlanOnly: true},
			{ResourceName: name, ImportState: true, ImportStateVerify: true},
			{
				PreConfig: func() {
					f.mu.Lock()
					defer f.mu.Unlock()
					if len(f.posts) != 1 {
						t.Error("plan or import mutated tags")
					}
					delete(f.membership, 17)
					f.tags[17] = "renamed externally"
				},
				Config: f.config(17),
				Check:  check(17, 2),
			},
			{Config: f.config(19), Check: check(19, 4)},
		},
	})
}

func TestMachineTagReadAndMutation(t *testing.T) {
	f := newMachineTagTestServer(t)
	r := f.resource(t)
	data := machineTag{SerialNumber: types.StringValue(f.serial), TagID: types.Int64Value(17)}
	ctx := context.Background()
	if present, _, err := r.membership(ctx, data); err != nil || present {
		t.Fatalf("initial read: present=%v, err=%v", present, err)
	}
	for range 2 {
		if err := r.mutate(ctx, data, "ADD"); err != nil {
			t.Fatal(err)
		}
	}
	if present, _, err := r.membership(ctx, data); err != nil || !present {
		t.Fatalf("read after idempotent add: present=%v, err=%v", present, err)
	}
	f.mu.Lock()
	f.tags[17] = "new tag name"
	f.mu.Unlock()
	if err := r.mutate(ctx, data, "REMOVE"); err != nil {
		t.Fatal(err)
	}
	if present, _, err := r.membership(ctx, data); err != nil || present {
		t.Fatalf("read after renamed tag removal: present=%v, err=%v", present, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !reflect.DeepEqual(f.membership, map[int]bool{18: true}) {
		t.Fatal("unrelated tag was changed")
	}
}

func TestMachineTagErrors(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f := newMachineTagTestServer(t)
			r := f.resource(t)
			data := machineTag{SerialNumber: types.StringValue(f.serial), TagID: types.Int64Value(17)}
			f.mu.Lock()
			f.getStatus = status
			f.mu.Unlock()
			_, actualStatus, err := r.membership(context.Background(), data)
			if err == nil || actualStatus != status || strings.Contains(err.Error(), "private-test-body") || strings.Contains(err.Error(), f.serial) {
				t.Fatalf("unexpected sanitized read error: status=%d, err=%v", actualStatus, err)
			}
			f.mu.Lock()
			f.postStatus = status
			f.mu.Unlock()
			err = r.mutate(context.Background(), data, "ADD")
			if err == nil || strings.Contains(err.Error(), "private-test-body") {
				t.Fatalf("unexpected sanitized mutation error: %v", err)
			}
		})
	}
}

func TestMachineTagDeletedTag(t *testing.T) {
	f := newMachineTagTestServer(t)
	r := f.resource(t)
	data := machineTag{SerialNumber: types.StringValue(f.serial), TagID: types.Int64Value(999)}
	if err := r.mutate(context.Background(), data, "REMOVE"); err != nil {
		t.Fatalf("deleted tag must make destroy idempotent: %v", err)
	}
	if err := r.mutate(context.Background(), data, "ADD"); err == nil || strings.Contains(err.Error(), "private-test-body") {
		t.Fatalf("missing tag must fail creation without leaking response: %v", err)
	}
	if len(f.posts) != 0 {
		t.Fatal("missing tag caused a write")
	}
}

func TestMachineTagInvalidImport(t *testing.T) {
	r := &MachineTagResource{}
	for _, id := range []string{"", "plain-serial", "/1", "YQ==/1", "YQ/0", "YQ/-1", "YQ/01", "YQ/+1", "YQ/1/2", "_w/1", "YR/1"} {
		t.Run(id, func(t *testing.T) {
			resp := frameworkresource.ImportStateResponse{}
			r.ImportState(context.Background(), frameworkresource.ImportStateRequest{ID: id}, &resp)
			if !resp.Diagnostics.HasError() {
				t.Fatal("invalid import accepted")
			}
		})
	}
}

func TestMachineTagRefresh(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		present bool
		removed bool
		failure bool
	}{
		{name: "present", present: true},
		{name: "missing association", removed: true},
		{name: "missing inventory", status: http.StatusNotFound, removed: true},
		{name: "forbidden preserves state", status: http.StatusForbidden, failure: true},
		{name: "server failure preserves state", status: http.StatusInternalServerError, failure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMachineTagTestServer(t)
			f.getStatus = tc.status
			if tc.present {
				f.membership[17] = true
			}
			r := f.resource(t)
			ctx := context.Background()
			var schemaResponse frameworkresource.SchemaResponse
			r.Schema(ctx, frameworkresource.SchemaRequest{}, &schemaResponse)
			state := tfsdk.State{Schema: schemaResponse.Schema}
			if diags := state.Set(ctx, machineTag{
				ID:           types.StringValue(machineTagID(f.serial, 17)),
				SerialNumber: types.StringValue(f.serial),
				TagID:        types.Int64Value(17),
			}); diags.HasError() {
				t.Fatal(diags)
			}
			resp := frameworkresource.ReadResponse{State: state}
			r.Read(ctx, frameworkresource.ReadRequest{State: state}, &resp)
			if resp.Diagnostics.HasError() != tc.failure || resp.State.Raw.IsNull() != tc.removed {
				t.Fatalf("unexpected read state: removed=%v diagnostics=%v", resp.State.Raw.IsNull(), resp.Diagnostics)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.posts) != 0 {
				t.Fatal("refresh wrote to the API")
			}
		})
	}
}

func TestMachineTagAdoptsExistingAssociation(t *testing.T) {
	f := newMachineTagTestServer(t)
	f.membership[17] = true
	r := f.resource(t)
	ctx := context.Background()
	var schemaResponse frameworkresource.SchemaResponse
	r.Schema(ctx, frameworkresource.SchemaRequest{}, &schemaResponse)
	plan := tfsdk.Plan{Schema: schemaResponse.Schema}
	if diags := plan.Set(ctx, machineTag{
		ID:           types.StringUnknown(),
		SerialNumber: types.StringValue(f.serial),
		TagID:        types.Int64Value(17),
	}); diags.HasError() {
		t.Fatal(diags)
	}
	resp := frameworkresource.CreateResponse{State: tfsdk.State{Schema: schemaResponse.Schema}}
	r.Create(ctx, frameworkresource.CreateRequest{Plan: plan}, &resp)
	if resp.Diagnostics.HasError() || resp.State.Raw.IsNull() {
		t.Fatalf("existing association was not adopted: %v", resp.Diagnostics)
	}
	var actual machineTag
	if diags := resp.State.Get(ctx, &actual); diags.HasError() || actual.ID.ValueString() != machineTagID(f.serial, 17) {
		t.Fatalf("incorrect adopted identity: %v", diags)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.posts) != 0 {
		t.Fatal("adopting existing association mutated tags")
	}
}

func TestMachineTagDeleteAfterInventoryDisappears(t *testing.T) {
	f := newMachineTagTestServer(t)
	f.membership[17] = true
	f.getStatus = http.StatusNotFound
	r := f.resource(t)
	ctx := context.Background()
	var schemaResponse frameworkresource.SchemaResponse
	r.Schema(ctx, frameworkresource.SchemaRequest{}, &schemaResponse)
	state := tfsdk.State{Schema: schemaResponse.Schema}
	if diags := state.Set(ctx, machineTag{
		ID:           types.StringValue(machineTagID(f.serial, 17)),
		SerialNumber: types.StringValue(f.serial),
		TagID:        types.Int64Value(17),
	}); diags.HasError() {
		t.Fatal(diags)
	}
	resp := frameworkresource.DeleteResponse{State: state}
	r.Delete(ctx, frameworkresource.DeleteRequest{State: state}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !reflect.DeepEqual(f.membership, map[int]bool{18: true}) || len(f.posts) != 1 || f.posts[0].Operations[0].Kind != "REMOVE" {
		t.Fatal("destroy must remove the association even after the inventory snapshot disappears")
	}
}
