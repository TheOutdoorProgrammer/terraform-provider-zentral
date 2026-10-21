package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/zentralopensource/goztl"
	"howett.net/plist"
)

type deviceNameTestServer struct {
	mu              sync.Mutex
	server          *httptest.Server
	devices         []namedDevice
	commands        map[string]deviceCommand
	native          []deviceCommand
	posts           []map[string]any
	requests        []string
	serial          string
	desired         string
	deviceID        int64
	itemStatus      string
	plistStatus     string
	commandStatus   string
	wrongSubmission bool
	wrongReadback   bool
	nameAtHistory   string
}

// Django's USE_TZ=false serializer emits naive UTC, unlike Go's time.Time encoder.
func deviceCommandResponse(command deviceCommand) map[string]any {
	var resultTime any
	if command.ResultTime != nil {
		resultTime = command.ResultTime.UTC().Format("2006-01-02T15:04:05.999999")
	}
	return map[string]any{"uuid": command.UUID, "enrolled_device": command.DeviceID, "status": command.Status, "result": command.Result, "result_time": resultTime}
}

func newDeviceNameTestServer(t *testing.T) *deviceNameTestServer {
	t.Helper()
	f := &deviceNameTestServer{serial: "test / serial 雪", desired: `Kid's <iPad> & "雪"`, deviceID: 42, commands: map[string]deviceCommand{}, itemStatus: "Acknowledged", plistStatus: "Acknowledged", commandStatus: "Acknowledged"}
	f.devices = []namedDevice{{ID: 42, Serial: f.serial, Name: "Old name", Platform: "iPadOS", Supervised: true}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, req.Method+" "+req.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if req.Header.Get("Authorization") != "Token fake-test-token" {
			t.Error("missing test authentication")
		}
		switch {
		case req.Method == "GET" && req.URL.Path == "/api/mdm/devices/":
			if req.URL.Query().Get("serial_number") != f.serial {
				t.Error("lookup did not preserve exact serial")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"count": len(f.devices), "results": f.devices})
		case req.Method == "GET" && req.URL.Path == "/api/mdm/devices/commands/":
			if f.nameAtHistory != "" {
				f.devices[0].Name = f.nameAtHistory
			}
			if req.URL.Query().Get("enrolled_device") != "42" || req.URL.Query().Get("name") != "DeviceInformation" {
				t.Error("native history not scoped to device and command")
			}
			commands := make([]map[string]any, len(f.native))
			for i, command := range f.native {
				commands[i] = deviceCommandResponse(command)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"count": len(f.native), "results": commands})
		case req.Method == "GET" && strings.HasPrefix(req.URL.Path, "/api/mdm/devices/commands/"):
			id := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/api/mdm/devices/commands/"), "/")
			command, ok := f.commands[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"detail":"private-test-secret"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(deviceCommandResponse(command))
		case req.Method == "POST" && req.URL.Path == "/api/mdm/devices/42/send_custom_command/":
			var body struct {
				Command string `json:"command"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			var command map[string]any
			if _, err := plist.Unmarshal([]byte(body.Command), &command); err != nil {
				t.Errorf("invalid command plist: %v", err)
				w.WriteHeader(400)
				return
			}
			f.posts = append(f.posts, command)
			result := map[string]any{"Status": f.plistStatus}
			switch command["RequestType"] {
			case "Settings":
				settings, ok := command["Settings"].([]any)
				if !ok || len(settings) != 1 {
					t.Error("expected exactly one setting")
					w.WriteHeader(400)
					return
				}
				setting, ok := settings[0].(map[string]any)
				if !ok || setting["Item"] != "DeviceName" || setting["DeviceName"] != f.desired {
					t.Error("rename payload changed or escaped name incorrectly")
				}
				result["Settings"] = []map[string]string{{"Item": "DeviceName", "Status": f.itemStatus}}
			case "DeviceInformation":
				if !reflect.DeepEqual(command["Queries"], []any{"DeviceName"}) {
					t.Error("verification did not request DeviceName independently")
				}
				name := f.desired
				if f.wrongReadback {
					name = "unexpected name"
				}
				result["QueryResponses"] = map[string]string{"DeviceName": name}
			default:
				t.Errorf("unexpected command %v", command["RequestType"])
			}
			encoded, err := plist.Marshal(result, plist.BinaryFormat)
			if err != nil {
				t.Error(err)
				return
			}
			now := time.Now().UTC()
			sent := deviceCommand{UUID: fmt.Sprintf("12345678-1234-4234-8234-%012d", len(f.posts)), DeviceID: f.deviceID, Status: f.commandStatus, Result: encoded, ResultTime: &now}
			f.commands[sent.UUID] = sent
			if f.wrongSubmission {
				sent.DeviceID++
			}
			_ = json.NewEncoder(w).Encode(deviceCommandResponse(sent))
		default:
			t.Errorf("unexpected API request %s %s", req.Method, req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func TestDeviceCommandResultTime(t *testing.T) {
	for _, tc := range []struct {
		name, timestamp, want string
		invalid               bool
	}{
		{name: "naive UTC microseconds", timestamp: `"2026-10-21T17:04:52.908618"`, want: "2026-10-21T17:04:52.908618Z"},
		{name: "naive UTC seconds", timestamp: `"2026-10-21T17:04:52"`, want: "2026-10-21T17:04:52Z"},
		{name: "UTC zone", timestamp: `"2026-10-21T17:04:52.908618Z"`, want: "2026-10-21T17:04:52.908618Z"},
		{name: "positive offset", timestamp: `"2026-10-21T19:04:52.908618+02:00"`, want: "2026-10-21T17:04:52.908618Z"},
		{name: "negative offset", timestamp: `"2026-10-21T13:04:52.908618-04:00"`, want: "2026-10-21T17:04:52.908618Z"},
		{name: "pending", timestamp: `null`},
		{name: "malformed", timestamp: `"private-invalid-timestamp"`, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var command deviceCommand
			err := json.Unmarshal([]byte(`{"uuid":"12345678-1234-4234-8234-000000000001","enrolled_device":42,"status":"Acknowledged","result":null,"result_time":`+tc.timestamp+`}`), &command)
			if tc.invalid {
				if err == nil || strings.Contains(err.Error(), "private-invalid-timestamp") {
					t.Fatalf("invalid timestamp must fail without echoing its value: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if command.UUID != "12345678-1234-4234-8234-000000000001" || command.DeviceID != 42 || command.Status != "Acknowledged" {
				t.Fatal("timestamp decoding changed the other command fields")
			}
			if tc.want == "" {
				if command.ResultTime != nil {
					t.Fatal("pending command acquired a result timestamp")
				}
				return
			}
			want, err := time.Parse(time.RFC3339Nano, tc.want)
			if err != nil || command.ResultTime == nil || !command.ResultTime.Equal(want) {
				t.Fatalf("wrong timestamp: got %v, want %v", command.ResultTime, want)
			}
		})
	}
}

func (f *deviceNameTestServer) resource(t *testing.T) *MDMDeviceNameResource {
	t.Helper()
	client, err := goztl.NewClient(f.server.Client(), f.server.URL+"/api/", "fake-test-token")
	if err != nil {
		t.Fatal(err)
	}
	return &MDMDeviceNameResource{client: client, pollInterval: time.Millisecond}
}

func (f *deviceNameTestServer) model() deviceNameModel {
	return deviceNameModel{ID: types.StringNull(), Serial: types.StringValue(f.serial), DeviceID: types.Int64Value(f.deviceID), Name: types.StringValue(f.desired), Timeout: types.Int64Value(2), CommandID: types.StringNull(), VerificationID: types.StringNull(), CommandName: types.StringNull()}
}

func (f *deviceNameTestServer) config() string {
	return fmt.Sprintf(`provider "zentral" {
 base_url = %q
 token = "fake-test-token"
}
resource "zentral_mdm_device_name" "test" {
 serial_number = %q
 enrolled_device_id = 42
 name = %q
 timeout_seconds = 2
}
`, f.server.URL+"/api/", f.serial, f.desired)
}

func TestMDMDeviceNameReconcileAndReadback(t *testing.T) {
	f := newDeviceNameTestServer(t)
	r := f.resource(t)
	data := f.model()
	checkpoints := 0
	if err := r.reconcile(context.Background(), &data, func() { checkpoints++ }); err != nil {
		t.Fatal(err)
	}
	if checkpoints < 2 || len(f.posts) != 2 || data.CommandID.ValueString() == "" || data.VerificationID.ValueString() == "" {
		t.Fatal("commands not checkpointed or independently verified")
	}
	if err := r.reconcile(context.Background(), &data, func() {}); err != nil {
		t.Fatal(err)
	}
	if len(f.posts) != 2 {
		t.Fatal("completed command was blindly replayed")
	}
	name, err := r.observedName(context.Background(), &data, &f.devices[0])
	if err != nil || name != f.desired {
		t.Fatalf("custom readback must win over stale inventory: name=%q err=%v", name, err)
	}
	now := time.Now().Add(time.Second)
	f.native = []deviceCommand{{UUID: "native", DeviceID: 42, Status: "Acknowledged", ResultTime: &now}}
	f.devices[0].Name = "Changed externally"
	name, err = r.observedName(context.Background(), &data, &f.devices[0])
	if err != nil || name != "Changed externally" {
		t.Fatalf("newer native inventory must reveal drift: name=%q err=%v", name, err)
	}
	if len(f.posts) != 2 {
		t.Fatal("read submitted commands")
	}
}

func TestMDMDeviceNameCanceledCommandRecovery(t *testing.T) {
	for _, changedTarget := range []bool{false, true} {
		t.Run(fmt.Sprint(changedTarget), func(t *testing.T) {
			f := newDeviceNameTestServer(t)
			r := f.resource(t)
			data := f.model()
			data.CommandID = types.StringValue("12345678-1234-4234-8234-000000000099")
			data.CommandName = data.Name
			if changedTarget {
				data.CommandName = types.StringValue("Previous target")
			}
			if err := r.reconcile(context.Background(), &data, func() {}); err != nil {
				t.Fatal(err)
			}
			if len(f.posts) != 2 || data.CommandName != data.Name {
				t.Fatal("canceled rename did not recover with a verified new command")
			}
		})
	}
}

func TestMDMDeviceNameNativeHistoryTimestampOrdering(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delta time.Duration
		drift bool
	}{
		{name: "older", delta: -time.Microsecond},
		{name: "same observation", drift: true},
		{name: "newer", delta: time.Microsecond, drift: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeviceNameTestServer(t)
			r := f.resource(t)
			data := f.model()
			if err := r.reconcile(context.Background(), &data, func() {}); err != nil {
				t.Fatal(err)
			}
			verifiedAt := time.Date(2026, 10, 21, 17, 4, 52, 908618000, time.UTC)
			verified := f.commands[data.VerificationID.ValueString()]
			verified.ResultTime = &verifiedAt
			f.commands[verified.UUID] = verified
			nativeAt := verifiedAt.Add(tc.delta)
			f.native = []deviceCommand{{UUID: "native", DeviceID: 42, Status: "Acknowledged", ResultTime: &nativeAt}}
			f.devices[0].Name = "Native inventory name"
			name, err := r.observedName(context.Background(), &data, &f.devices[0])
			want := f.desired
			if tc.drift {
				want = f.devices[0].Name
			}
			if err != nil || name != want || len(f.posts) != 2 {
				t.Fatalf("wrong read-only history result: name=%q err=%v", name, err)
			}
		})
	}
}

func TestMDMDeviceNameResumeAcknowledgedNaiveTimestamp(t *testing.T) {
	f := newDeviceNameTestServer(t)
	r := f.resource(t)
	data := f.model()
	commandID := "12345678-1234-4234-8234-000000000099"
	result, err := plist.Marshal(map[string]any{"Status": "Acknowledged", "Settings": []map[string]string{{"Item": "DeviceName", "Status": "Acknowledged"}}}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	acknowledgedAt := time.Date(2026, 10, 21, 17, 4, 52, 908618000, time.UTC)
	f.commands[commandID] = deviceCommand{UUID: commandID, DeviceID: 42, Status: "Acknowledged", Result: result, ResultTime: &acknowledgedAt}
	data.CommandID = types.StringValue(commandID)
	data.CommandName = data.Name
	if err := r.reconcile(context.Background(), &data, func() {}); err != nil {
		t.Fatal(err)
	}
	if data.CommandID.ValueString() != commandID || len(f.posts) != 1 || f.posts[0]["RequestType"] != "DeviceInformation" {
		t.Fatal("resuming an acknowledged command replayed the device rename")
	}
}

func TestMDMDeviceNameCanceledVerificationRecovery(t *testing.T) {
	for _, refreshFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(refreshFirst), func(t *testing.T) {
			f := newDeviceNameTestServer(t)
			r := f.resource(t)
			ctx := context.Background()
			data := f.model()
			if err := r.reconcile(ctx, &data, func() {}); err != nil {
				t.Fatal(err)
			}
			delete(f.commands, data.VerificationID.ValueString())
			if refreshFirst {
				var schemaResp frameworkresource.SchemaResponse
				r.Schema(ctx, frameworkresource.SchemaRequest{}, &schemaResp)
				state := tfsdk.State{Schema: schemaResp.Schema}
				if diags := state.Set(ctx, &data); diags.HasError() {
					t.Fatal(diags)
				}
				response := frameworkresource.ReadResponse{State: state}
				r.Read(ctx, frameworkresource.ReadRequest{State: state}, &response)
				if response.Diagnostics.HasError() {
					t.Fatal(response.Diagnostics)
				}
				if diags := response.State.Get(ctx, &data); diags.HasError() {
					t.Fatal(diags)
				}
				if !data.VerificationID.IsNull() || len(f.posts) != 2 {
					t.Fatal("refresh did not forget canceled verification without a mutation")
				}
				data.Name = types.StringValue(f.desired)
			}
			if err := r.reconcile(ctx, &data, func() {}); err != nil {
				t.Fatal(err)
			}
			if len(f.posts) != 3 || f.posts[2]["RequestType"] != "DeviceInformation" {
				t.Fatal("recovery replayed a rename instead of replacing the canceled readback")
			}
		})
	}
}

func TestMDMDeviceNameRefreshObservesConcurrentInventoryUpdate(t *testing.T) {
	f := newDeviceNameTestServer(t)
	r := f.resource(t)
	ctx := context.Background()
	data := f.model()
	if err := r.reconcile(ctx, &data, func() {}); err != nil {
		t.Fatal(err)
	}
	initial, err := r.lookup(ctx, &data)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Second)
	f.native = []deviceCommand{{UUID: "native", DeviceID: 42, Status: "Acknowledged", ResultTime: &now}}
	f.nameAtHistory = f.desired
	name, err := r.observedName(ctx, &data, initial)
	if err != nil || name != f.desired || len(f.posts) != 2 {
		t.Fatalf("refresh used stale inventory or sent commands: %v", err)
	}
}

func TestMDMDeviceNameIdentityAndSafetyGuards(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*deviceNameTestServer)
		wantErr bool
	}{
		{"historical first", func(f *deviceNameTestServer) {
			old := f.devices[0]
			old.ID = 7
			f.devices = append([]namedDevice{old}, f.devices...)
		}, false},
		{"wrong serial", func(f *deviceNameTestServer) { f.devices[0].Serial = "another serial" }, true},
		{"wrong enrollment", func(f *deviceNameTestServer) { f.devices[0].ID = 43 }, true},
		{"checked out", func(f *deviceNameTestServer) { value := "2026-01-01"; f.devices[0].Checkout = &value }, true},
		{"blocked", func(f *deviceNameTestServer) { value := "2026-01-01"; f.devices[0].Blocked = &value }, true},
		{"unsupervised", func(f *deviceNameTestServer) { f.devices[0].Supervised = false }, true},
		{"already named", func(f *deviceNameTestServer) { f.devices[0].Name = f.desired }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeviceNameTestServer(t)
			tc.change(f)
			r := f.resource(t)
			data := f.model()
			err := r.reconcile(context.Background(), &data, func() {})
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected error: %v", err)
			}
			if (tc.wantErr || tc.name == "already named") && len(f.posts) != 0 {
				t.Fatal("unsafe or unnecessary command submitted")
			}
		})
	}
}

func TestMDMDeviceNameAcknowledgementFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*deviceNameTestServer)
		posts  int
	}{
		{"item rejected", func(f *deviceNameTestServer) { f.itemStatus = "Error" }, 1},
		{"plist rejected", func(f *deviceNameTestServer) { f.plistStatus = "Error" }, 1},
		{"command rejected", func(f *deviceNameTestServer) { f.commandStatus = "Error" }, 1},
		{"wrong submit device", func(f *deviceNameTestServer) { f.wrongSubmission = true }, 1},
		{"wrong readback", func(f *deviceNameTestServer) { f.wrongReadback = true }, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeviceNameTestServer(t)
			tc.change(f)
			r := f.resource(t)
			data := f.model()
			err := r.reconcile(context.Background(), &data, func() {})
			if err == nil || len(f.posts) != tc.posts {
				t.Fatalf("expected failure after %d posts, got %d: %v", tc.posts, len(f.posts), err)
			}
			if strings.Contains(err.Error(), f.serial) || strings.Contains(err.Error(), f.desired) {
				t.Fatal("diagnostic exposes device data")
			}
		})
	}
}

func TestMDMDeviceNameMalformedResults(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command deviceCommand
	}{
		{"unacknowledged", deviceCommand{Status: "Error", Result: []byte("private-test-result")}},
		{"missing result", deviceCommand{Status: "Acknowledged"}},
		{"invalid plist", deviceCommand{Status: "Acknowledged", Result: []byte("private-test-result")}},
		{"missing status", deviceCommand{Status: "Acknowledged", Result: []byte(`<?xml version="1.0"?><plist version="1.0"><dict><key>DeviceName</key><string>private-test-name</string></dict></plist>`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeDeviceResult(&tc.command)
			if err == nil || strings.Contains(err.Error(), "private-test") {
				t.Fatalf("missing or unsafe result error: %v", err)
			}
		})
	}
}

func TestMDMDeviceNameInterruptedCommandResume(t *testing.T) {
	f := newDeviceNameTestServer(t)
	f.commandStatus = "NotNow"
	r := f.resource(t)
	data := f.model()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	err := r.reconcile(ctx, &data, func() {})
	if !errors.Is(err, context.DeadlineExceeded) || data.CommandID.ValueString() == "" || len(f.posts) != 1 {
		t.Fatalf("pending command not retained: %v", err)
	}
	f.mu.Lock()
	saved := f.commands[data.CommandID.ValueString()]
	saved.Status = "Acknowledged"
	f.commands[saved.UUID] = saved
	f.commandStatus = "Acknowledged"
	f.mu.Unlock()
	if err := r.reconcile(context.Background(), &data, func() {}); err != nil {
		t.Fatal(err)
	}
	if len(f.posts) != 2 || f.posts[1]["RequestType"] != "DeviceInformation" {
		t.Fatal("retry replayed pending rename")
	}
}

func TestMDMDeviceNameWrongCommandIdentity(t *testing.T) {
	f := newDeviceNameTestServer(t)
	r := f.resource(t)
	const savedID = "12345678-1234-4234-8234-000000000001"
	for _, command := range []deviceCommand{{UUID: "12345678-1234-4234-8234-000000000002", DeviceID: 42}, {UUID: savedID, DeviceID: 99}} {
		f.commands[savedID] = command
		if _, err := r.getCommand(context.Background(), savedID, 42); err == nil {
			t.Fatal("wrong command identity accepted")
		}
	}
	before := len(f.requests)
	for _, id := range []string{"", "../devices", "saved?token=private", "saved#fragment"} {
		if _, err := r.getCommand(context.Background(), id, 42); err == nil {
			t.Fatal("invalid saved ID accepted")
		}
	}
	if len(f.requests) != before {
		t.Fatal("invalid ID reached API")
	}
}

func TestMDMDeviceNameReadAndDeleteDoNotSendCommands(t *testing.T) {
	f := newDeviceNameTestServer(t)
	r := f.resource(t)
	data := f.model()
	var schemaResp frameworkresource.SchemaResponse
	r.Schema(context.Background(), frameworkresource.SchemaRequest{}, &schemaResp)
	state := tfsdk.State{Schema: schemaResp.Schema}
	if diags := state.Set(context.Background(), &data); diags.HasError() {
		t.Fatal(diags)
	}
	response := frameworkresource.ReadResponse{State: state}
	r.Read(context.Background(), frameworkresource.ReadRequest{State: state}, &response)
	if response.Diagnostics.HasError() {
		t.Fatal(response.Diagnostics)
	}
	before := len(f.requests)
	r.Delete(context.Background(), frameworkresource.DeleteRequest{State: response.State}, &frameworkresource.DeleteResponse{})
	if len(f.requests) != before || len(f.posts) != 0 {
		t.Fatal("read or delete issued mutation")
	}
}

func TestMDMDeviceNameInvalidImport(t *testing.T) {
	r := &MDMDeviceNameResource{}
	for _, id := range []string{"", "plain-serial", "/1", "YQ==/1", "YQ/0", "YQ/-1", "YQ/01", "YQ/+1", "YQ/1/2", "_w/1", "YR/1"} {
		t.Run(id, func(t *testing.T) {
			var schemaResp frameworkresource.SchemaResponse
			r.Schema(context.Background(), frameworkresource.SchemaRequest{}, &schemaResp)
			response := frameworkresource.ImportStateResponse{State: tfsdk.State{Schema: schemaResp.Schema}}
			r.ImportState(context.Background(), frameworkresource.ImportStateRequest{ID: id}, &response)
			if !response.Diagnostics.HasError() {
				t.Fatal("invalid import accepted")
			}
		})
	}
}

func TestMDMDeviceNameImport(t *testing.T) {
	f := newDeviceNameTestServer(t)
	r := f.resource(t)
	ctx := context.Background()
	var schemaResp frameworkresource.SchemaResponse
	r.Schema(ctx, frameworkresource.SchemaRequest{}, &schemaResp)
	response := frameworkresource.ImportStateResponse{State: tfsdk.State{
		Schema: schemaResp.Schema,
		Raw:    tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), nil),
	}}
	id := deviceNameID(f.serial, f.deviceID)
	r.ImportState(ctx, frameworkresource.ImportStateRequest{ID: id}, &response)
	if response.Diagnostics.HasError() {
		t.Fatal(response.Diagnostics)
	}
	read := frameworkresource.ReadResponse{State: response.State}
	r.Read(ctx, frameworkresource.ReadRequest{State: response.State}, &read)
	if read.Diagnostics.HasError() {
		t.Fatal(read.Diagnostics)
	}
	var got deviceNameModel
	if diags := read.State.Get(ctx, &got); diags.HasError() {
		t.Fatal(diags)
	}
	if got.ID.ValueString() != id || got.Serial.ValueString() != f.serial || got.DeviceID.ValueInt64() != 42 || got.Name.ValueString() != "Old name" || len(f.posts) != 0 {
		t.Fatal("import failed to read the exact identity without mutation")
	}
}

func TestMDMDeviceNamePartialUpdatePreservesPendingCommand(t *testing.T) {
	f := newDeviceNameTestServer(t)
	r := f.resource(t)
	ctx := context.Background()
	data := f.model()
	if err := r.reconcile(ctx, &data, func() {}); err != nil {
		t.Fatal(err)
	}
	var schemaResp frameworkresource.SchemaResponse
	r.Schema(ctx, frameworkresource.SchemaRequest{}, &schemaResp)
	state := tfsdk.State{Schema: schemaResp.Schema}
	if diags := state.Set(ctx, &data); diags.HasError() {
		t.Fatal(diags)
	}
	f.desired = "Updated name"
	f.commandStatus = "NotNow"
	data.Name = types.StringValue(f.desired)
	plan := tfsdk.Plan{Schema: schemaResp.Schema}
	if diags := plan.Set(ctx, &data); diags.HasError() {
		t.Fatal(diags)
	}
	response := frameworkresource.UpdateResponse{State: state}
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	r.Update(short, frameworkresource.UpdateRequest{State: state, Plan: plan}, &response)
	if !response.Diagnostics.HasError() {
		t.Fatal("pending update unexpectedly completed")
	}
	var pending deviceNameModel
	if diags := response.State.Get(ctx, &pending); diags.HasError() {
		t.Fatal(diags)
	}
	if pending.CommandName.ValueString() != f.desired || pending.CommandID == data.CommandID || len(f.posts) != 3 {
		t.Fatal("failed update did not checkpoint the new command")
	}
	read := frameworkresource.ReadResponse{State: response.State}
	r.Read(ctx, frameworkresource.ReadRequest{State: response.State}, &read)
	if read.Diagnostics.HasError() {
		t.Fatal(read.Diagnostics)
	}
	var refreshed deviceNameModel
	if diags := read.State.Get(ctx, &refreshed); diags.HasError() {
		t.Fatal(diags)
	}
	if refreshed.CommandID != pending.CommandID || refreshed.CommandName != pending.CommandName {
		t.Fatal("read discarded a pending rename")
	}
	f.mu.Lock()
	command := f.commands[pending.CommandID.ValueString()]
	command.Status = "Acknowledged"
	f.commands[command.UUID] = command
	f.commandStatus = "Acknowledged"
	f.mu.Unlock()
	resumed := frameworkresource.UpdateResponse{State: read.State}
	r.Update(ctx, frameworkresource.UpdateRequest{State: read.State, Plan: plan}, &resumed)
	if resumed.Diagnostics.HasError() {
		t.Fatal(resumed.Diagnostics)
	}
	if len(f.posts) != 4 || f.posts[3]["RequestType"] != "DeviceInformation" {
		t.Fatal("resumed update replayed the rename instead of verifying it")
	}
}

func TestMDMDeviceNameReadPreservesPendingVerification(t *testing.T) {
	f := newDeviceNameTestServer(t)
	r := f.resource(t)
	ctx := context.Background()
	data := f.model()
	if err := r.reconcile(ctx, &data, func() {}); err != nil {
		t.Fatal(err)
	}
	command := f.commands[data.VerificationID.ValueString()]
	command.Status = "NotNow"
	f.commands[command.UUID] = command
	var schemaResp frameworkresource.SchemaResponse
	r.Schema(ctx, frameworkresource.SchemaRequest{}, &schemaResp)
	state := tfsdk.State{Schema: schemaResp.Schema}
	if diags := state.Set(ctx, &data); diags.HasError() {
		t.Fatal(diags)
	}
	response := frameworkresource.ReadResponse{State: state}
	r.Read(ctx, frameworkresource.ReadRequest{State: state}, &response)
	if response.Diagnostics.HasError() {
		t.Fatal(response.Diagnostics)
	}
	var got deviceNameModel
	if diags := response.State.Get(ctx, &got); diags.HasError() {
		t.Fatal(diags)
	}
	if got.VerificationID != data.VerificationID || got.CommandID != data.CommandID || got.CommandName != data.CommandName || len(f.posts) != 2 {
		t.Fatal("refresh discarded a pending verification or submitted a command")
	}
}

func TestMDMDeviceNameResourceLifecycle(t *testing.T) {
	f := newDeviceNameTestServer(t)
	checkPosts := func(expected int) resource.TestCheckFunc {
		return func(_ *terraform.State) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.posts) != expected {
				return fmt.Errorf("expected %d commands, got %d", expected, len(f.posts))
			}
			return nil
		}
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkPosts(4),
		Steps: []resource.TestStep{
			{Config: f.config(), Check: resource.ComposeAggregateTestCheckFunc(resource.TestCheckResourceAttr("zentral_mdm_device_name.test", "name", f.desired), checkPosts(2))},
			{Config: f.config(), PlanOnly: true},
			{Config: f.config(), Check: checkPosts(2)},
			{PreConfig: func() {
				f.mu.Lock()
				defer f.mu.Unlock()
				now := time.Now()
				f.devices[0].Name = "External drift"
				f.native = []deviceCommand{{UUID: "native", DeviceID: 42, Status: "Acknowledged", ResultTime: &now}}
			}, Config: f.config(), Check: checkPosts(4)},
		},
	})
}
