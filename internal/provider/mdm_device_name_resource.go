package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/zentralopensource/goztl"
	"howett.net/plist"
)

var _ resource.Resource = &MDMDeviceNameResource{}
var _ resource.ResourceWithImportState = &MDMDeviceNameResource{}

var errDeviceCommandNotFound = errors.New("saved device command no longer exists")

type MDMDeviceNameResource struct {
	client       *goztl.Client
	pollInterval time.Duration
}

type deviceNameModel struct {
	ID             types.String `tfsdk:"id"`
	Serial         types.String `tfsdk:"serial_number"`
	DeviceID       types.Int64  `tfsdk:"enrolled_device_id"`
	Name           types.String `tfsdk:"name"`
	Timeout        types.Int64  `tfsdk:"timeout_seconds"`
	CommandID      types.String `tfsdk:"command_id"`
	VerificationID types.String `tfsdk:"verification_command_id"`
	CommandName    types.String `tfsdk:"command_name"`
}

type namedDevice struct {
	ID         int64   `json:"id"`
	Serial     string  `json:"serial_number"`
	Name       string  `json:"name"`
	Checkout   *string `json:"checkout_at"`
	Blocked    *string `json:"blocked_at"`
	Supervised bool    `json:"supervised"`
	Platform   string  `json:"platform"`
}

type deviceCommand struct {
	UUID       string     `json:"uuid"`
	DeviceID   int64      `json:"enrolled_device"`
	Status     string     `json:"status"`
	Result     []byte     `json:"result"`
	ResultTime *time.Time `json:"result_time"`
}

func (c *deviceCommand) UnmarshalJSON(data []byte) error {
	type command deviceCommand
	var decoded struct {
		command
		ResultTime json.RawMessage `json:"result_time"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if len(decoded.ResultTime) != 0 && string(decoded.ResultTime) != "null" {
		var timestamp time.Time
		if err := json.Unmarshal(decoded.ResultTime, &timestamp); err != nil {
			// Zentral uses naive UTC with USE_TZ=false; the SDK also supports
			// that representation, while time.Time handles timezone-aware APIs.
			var sdkTimestamp goztl.Timestamp
			if err := json.Unmarshal(decoded.ResultTime, &sdkTimestamp); err != nil {
				return errors.New("invalid command result timestamp")
			}
			timestamp = sdkTimestamp.Time.UTC()
		}
		decoded.command.ResultTime = &timestamp
	}
	*c = deviceCommand(decoded.command)
	return nil
}

type deviceCommandResult struct {
	Status   string `plist:"Status"`
	Settings []struct {
		Item   string `plist:"Item"`
		Status string `plist:"Status"`
	} `plist:"Settings"`
	QueryResponses struct {
		DeviceName string `plist:"DeviceName"`
	} `plist:"QueryResponses"`
}

func NewMDMDeviceNameResource() resource.Resource {
	return &MDMDeviceNameResource{pollInterval: 2 * time.Second}
}

func (r *MDMDeviceNameResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_mdm_device_name"
}

func (r *MDMDeviceNameResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	computedID := func(description string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, Description: description}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the name of one enrolled Apple device. Apply waits for the Settings item acknowledgement and an independent DeviceInformation readback. Read never sends commands. Destroy only forgets the name; it does not rename or erase the device. Device inventory can lag until its next DeviceInformation response.",
		Attributes: map[string]schema.Attribute{
			"id":                      computedID("The base64url-encoded serial number and enrollment ID, separated by a slash."),
			"serial_number":           schema.StringAttribute{Required: true, Description: "Exact device serial number, used to guard the enrollment ID.", Validators: []validator.String{stringvalidator.LengthAtLeast(1)}, PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"enrolled_device_id":      schema.Int64Attribute{Required: true, Description: "Exact active Zentral enrollment ID. Re-enrollment requires updating this identity.", Validators: []validator.Int64{int64validator.AtLeast(1)}, PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace()}},
			"name":                    schema.StringAttribute{Required: true, Description: "Desired device name. Stored in state; protect state as personal data.", Validators: []validator.String{stringvalidator.LengthBetween(1, 255), stringvalidator.RegexMatches(regexp.MustCompile(`^[^\x00-\x1f\x7f]+$`), "must not contain control characters")}},
			"timeout_seconds":         schema.Int64Attribute{Optional: true, Computed: true, Default: int64default.StaticInt64(120), Description: "Maximum apply wait for both commands, in seconds. Offline devices may time out with commands still queued.", Validators: []validator.Int64{int64validator.Between(1, 3600)}},
			"command_id":              computedID("Last rename command UUID, retained for recovery."),
			"verification_command_id": computedID("Last DeviceInformation verification UUID, retained for recovery."),
			"command_name":            computedID("Name targeted by the retained command. Stored to resume interrupted updates without blindly replaying a rename."),
		},
	}
}

func (r *MDMDeviceNameResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*goztl.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider client", "Expected a Zentral API client.")
		return
	}
	r.client = client
}

func deviceNameID(serial string, id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(serial)) + "/" + strconv.FormatInt(id, 10)
}

func (r *MDMDeviceNameResource) lookup(ctx context.Context, data *deviceNameModel) (*namedDevice, error) {
	var page struct {
		Count   int           `json:"count"`
		Results []namedDevice `json:"results"`
	}
	_, err := deviceRequest(ctx, r.client, "GET", "mdm/devices/?"+url.Values{"serial_number": {data.Serial.ValueString()}, "limit": {"1000"}}.Encode(), nil, &page)
	if err != nil {
		return nil, err
	}
	// A serial may have historical enrollments. Never choose the first matching row.
	if page.Count > len(page.Results) {
		return nil, errors.New("device lookup exceeded its bounded result size")
	}
	for _, d := range page.Results {
		if d.ID == data.DeviceID.ValueInt64() && d.Serial == data.Serial.ValueString() && d.Checkout == nil {
			return &d, nil
		}
	}
	return nil, nil
}

func (r *MDMDeviceNameResource) getCommand(ctx context.Context, id string, deviceID int64) (*deviceCommand, error) {
	if !regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`).MatchString(id) {
		return nil, errors.New("invalid saved command identifier")
	}
	var command deviceCommand
	status, err := deviceRequest(ctx, r.client, "GET", "mdm/devices/commands/"+id+"/", nil, &command)
	if status == 404 {
		return nil, errDeviceCommandNotFound
	}
	if err != nil {
		return nil, err
	}
	if command.UUID != id || command.DeviceID != deviceID {
		return nil, errors.New("command response did not match the intended device and command")
	}
	return &command, nil
}

func decodeDeviceResult(command *deviceCommand) (*deviceCommandResult, error) {
	if command.Status != "Acknowledged" {
		return nil, errors.New("device command was not acknowledged")
	}
	var result deviceCommandResult
	if _, err := plist.Unmarshal(command.Result, &result); err != nil || result.Status != "Acknowledged" {
		return nil, errors.New("missing or invalid acknowledged device result")
	}
	return &result, nil
}

func (r *MDMDeviceNameResource) waitCommand(ctx context.Context, id string, deviceID int64) (*deviceCommandResult, error) {
	for {
		command, err := r.getCommand(ctx, id, deviceID)
		if err != nil {
			return nil, err
		}
		switch command.Status {
		case "Acknowledged":
			return decodeDeviceResult(command)
		case "", "NotNow":
		default:
			return nil, errors.New("device rejected the command; inspect its protected command result in Zentral")
		}
		interval := r.pollInterval
		if interval <= 0 {
			interval = 2 * time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *MDMDeviceNameResource) send(ctx context.Context, data *deviceNameModel, command any) (string, error) {
	payload, err := plist.Marshal(command, plist.XMLFormat)
	if err != nil {
		return "", errors.New("could not encode device command")
	}
	var sent deviceCommand
	_, err = deviceRequest(ctx, r.client, "POST", fmt.Sprintf("mdm/devices/%d/send_custom_command/", data.DeviceID.ValueInt64()), map[string]string{"command": string(payload)}, &sent)
	if err != nil {
		return "", err
	}
	if sent.UUID == "" || sent.DeviceID != data.DeviceID.ValueInt64() {
		return "", errors.New("command submission returned an unexpected device or missing identifier")
	}
	return sent.UUID, nil
}

func (r *MDMDeviceNameResource) reconcile(ctx context.Context, data *deviceNameModel, checkpoint func()) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(data.Timeout.ValueInt64())*time.Second)
	defer cancel()
	d, err := r.lookup(ctx, data)
	if err != nil {
		return err
	}
	if d == nil {
		return errors.New("no active enrollment matches both the serial number and enrollment ID")
	}
	if d.Blocked != nil {
		return errors.New("device is blocked; unblock it separately before renaming")
	}
	if (d.Platform == "iOS" || d.Platform == "iPadOS") && !d.Supervised {
		return errors.New("renaming this mobile device requires supervision")
	}
	data.ID = types.StringValue(deviceNameID(data.Serial.ValueString(), data.DeviceID.ValueInt64()))
	if data.CommandID.ValueString() != "" {
		previous, err := r.getCommand(ctx, data.CommandID.ValueString(), data.DeviceID.ValueInt64())
		if errors.Is(err, errDeviceCommandNotFound) {
			// Zentral cancellation deletes an unsent command instead of retaining a status.
			data.CommandID = types.StringNull()
			data.VerificationID = types.StringNull()
			data.CommandName = types.StringNull()
			checkpoint()
		} else if err != nil {
			return err
		} else if data.CommandName.ValueString() != data.Name.ValueString() && (previous.Status == "" || previous.Status == "NotNow") {
			return errors.New("previous rename is still pending; let it complete or cancel it in Zentral before changing the target name")
		}
	}
	if data.CommandName.ValueString() != data.Name.ValueString() {
		data.CommandID = types.StringNull()
		data.VerificationID = types.StringNull()
		data.CommandName = types.StringNull()
	}
	if data.CommandID.ValueString() == "" {
		if d.Name == data.Name.ValueString() {
			checkpoint()
			return nil
		}
		id, err := r.send(ctx, data, map[string]any{"RequestType": "Settings", "Settings": []map[string]string{{"Item": "DeviceName", "DeviceName": data.Name.ValueString()}}})
		if err != nil {
			return err
		}
		data.CommandID = types.StringValue(id)
		data.CommandName = data.Name
		data.VerificationID = types.StringNull()
		checkpoint()
	}
	result, err := r.waitCommand(ctx, data.CommandID.ValueString(), data.DeviceID.ValueInt64())
	if err != nil {
		return err
	}
	acknowledged := false
	for _, setting := range result.Settings {
		if setting.Item == "DeviceName" {
			if setting.Status != "Acknowledged" {
				return errors.New("DeviceName setting was rejected even though the command was acknowledged")
			}
			acknowledged = true
		}
	}
	if !acknowledged {
		return errors.New("device response did not acknowledge the DeviceName setting")
	}
	if data.VerificationID.ValueString() != "" {
		_, err := r.getCommand(ctx, data.VerificationID.ValueString(), data.DeviceID.ValueInt64())
		if errors.Is(err, errDeviceCommandNotFound) {
			data.VerificationID = types.StringNull()
			checkpoint()
		} else if err != nil {
			return err
		}
	}
	if data.VerificationID.ValueString() == "" {
		id, err := r.send(ctx, data, map[string]any{"RequestType": "DeviceInformation", "Queries": []string{"DeviceName"}})
		if err != nil {
			return err
		}
		data.VerificationID = types.StringValue(id)
		checkpoint()
	}
	result, err = r.waitCommand(ctx, data.VerificationID.ValueString(), data.DeviceID.ValueInt64())
	if err != nil {
		return err
	}
	if result.QueryResponses.DeviceName != data.Name.ValueString() {
		return errors.New("device readback does not match the requested name")
	}
	return nil
}

func (r *MDMDeviceNameResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data deviceNameModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.CommandID = types.StringNull()
	data.VerificationID = types.StringNull()
	data.CommandName = types.StringNull()
	if err := r.reconcile(ctx, &data, func() { resp.Diagnostics.Append(resp.State.Set(ctx, &data)...) }); err != nil {
		resp.Diagnostics.AddError("Device rename incomplete", err.Error()+". A submitted command may still execute. Inspect retained command IDs before retrying or replacing this resource.")
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *MDMDeviceNameResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data, previous deviceNameModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &previous)...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.CommandID = previous.CommandID
	data.VerificationID = previous.VerificationID
	data.CommandName = previous.CommandName
	if err := r.reconcile(ctx, &data, func() { resp.Diagnostics.Append(resp.State.Set(ctx, &data)...) }); err != nil {
		resp.Diagnostics.AddError("Device rename incomplete", err.Error()+". A submitted command may still execute. Inspect retained command IDs before retrying.")
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// Custom DeviceInformation commands do not update Zentral's inventory name. Use their
// verified result until a newer native DeviceInformation observation supersedes it.
func (r *MDMDeviceNameResource) observedName(ctx context.Context, data *deviceNameModel, d *namedDevice) (string, error) {
	if data.VerificationID.ValueString() == "" {
		return d.Name, nil
	}
	verified, err := r.getCommand(ctx, data.VerificationID.ValueString(), d.ID)
	if errors.Is(err, errDeviceCommandNotFound) {
		data.VerificationID = types.StringNull()
		return d.Name, nil
	}
	if err != nil {
		return "", err
	}
	if verified.Status != "Acknowledged" {
		return d.Name, nil
	}
	result, err := decodeDeviceResult(verified)
	if err != nil {
		return "", err
	}
	if verified.ResultTime == nil || result.QueryResponses.DeviceName == "" {
		return "", errors.New("verification response is missing a timestamp or device name")
	}
	for offset := 0; offset < 10000; offset += 1000 {
		var page struct {
			Count   int             `json:"count"`
			Results []deviceCommand `json:"results"`
		}
		_, err := deviceRequest(ctx, r.client, "GET", "mdm/devices/commands/?"+url.Values{"name": {"DeviceInformation"}, "enrolled_device": {strconv.FormatInt(d.ID, 10)}, "limit": {"1000"}, "offset": {strconv.Itoa(offset)}}.Encode(), nil, &page)
		if err != nil {
			return "", err
		}
		for _, c := range page.Results {
			if c.DeviceID == d.ID && c.Status == "Acknowledged" && c.ResultTime != nil && !c.ResultTime.Before(*verified.ResultTime) {
				// The native command may have completed after the initial inventory lookup.
				current, err := r.lookup(ctx, data)
				if err != nil {
					return "", err
				}
				if current == nil {
					return "", errors.New("device enrollment disappeared during refresh; refresh again")
				}
				return current.Name, nil
			}
		}
		if offset+len(page.Results) >= page.Count {
			return result.QueryResponses.DeviceName, nil
		}
		if len(page.Results) == 0 {
			break
		}
	}
	return "", errors.New("device command history exceeded its bounded read size")
}

func (r *MDMDeviceNameResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data deviceNameModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	d, err := r.lookup(ctx, &data)
	if err != nil {
		resp.Diagnostics.AddError("Device name lookup failed", err.Error())
		return
	}
	if d == nil {
		resp.State.RemoveResource(ctx)
		return
	}
	name, err := r.observedName(ctx, &data, d)
	if err != nil {
		resp.Diagnostics.AddError("Device name read failed", err.Error())
		return
	}
	data.Name = types.StringValue(name)
	// Only a completed readback superseded by newer inventory establishes drift.
	// A still-pending readback must retain its UUID so apply can resume it.
	if name != data.CommandName.ValueString() && data.VerificationID.ValueString() != "" {
		verified, err := r.getCommand(ctx, data.VerificationID.ValueString(), d.ID)
		if err != nil {
			resp.Diagnostics.AddError("Device name read failed", err.Error())
			return
		}
		if verified.Status == "Acknowledged" {
			data.CommandID = types.StringNull()
			data.VerificationID = types.StringNull()
			data.CommandName = types.StringNull()
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *MDMDeviceNameResource) Delete(context.Context, resource.DeleteRequest, *resource.DeleteResponse) {
}

func (r *MDMDeviceNameResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, "/")
	if len(parts) != 2 {
		resp.Diagnostics.AddError("Invalid import ID", "Expected base64url(serial_number)/enrolled_device_id.")
		return
	}
	serial, err := base64.RawURLEncoding.DecodeString(parts[0])
	id, idErr := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || len(serial) == 0 || !utf8.Valid(serial) || strings.TrimSpace(string(serial)) != string(serial) || idErr != nil || id <= 0 || deviceNameID(string(serial), id) != req.ID {
		resp.Diagnostics.AddError("Invalid import ID", "Expected canonical base64url(serial_number)/positive_enrolled_device_id.")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("serial_number"), string(serial))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("enrolled_device_id"), id)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("timeout_seconds"), int64(120))...)
}
