package provider

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/zentralopensource/goztl"
)

var _ resource.Resource = &MachineTagResource{}
var _ resource.ResourceWithImportState = &MachineTagResource{}

type MachineTagResource struct {
	client *goztl.Client
}

type machineTag struct {
	ID           types.String `tfsdk:"id"`
	SerialNumber types.String `tfsdk:"serial_number"`
	TagID        types.Int64  `tfsdk:"tag_id"`
}

type machineTagOperation struct {
	Kind  string   `json:"kind"`
	Names []string `json:"names"`
}

type machineTagRequest struct {
	SerialNumbers []string              `json:"serial_numbers"`
	Operations    []machineTagOperation `json:"operations"`
}

func NewMachineTagResource() resource.Resource {
	return &MachineTagResource{}
}

func (r *MachineTagResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_machine_tag"
}

func (r *MachineTagResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages one direct inventory tag association for an existing machine. Other tags are preserved. Changing either identity attribute replaces the association. Import an existing association before managing it; destroy removes that association even if it existed before creation. Requires inventory.view_machinesnapshot, inventory.view_tag, inventory.add_tag, inventory.add_taxonomy, inventory.add_machinetag, and inventory.delete_machinetag API permissions.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Association identifier: unpadded base64url-encoded serial number, a slash, and the tag ID.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"serial_number": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Exact inventory serial number of the machine. Requires replacement when changed. Stored in state.",
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"tag_id": schema.Int64Attribute{
				Required:            true,
				MarkdownDescription: "ID of an existing inventory tag. Requires replacement when changed.",
				Validators:          []validator.Int64{int64validator.AtLeast(1)},
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
		},
	}
}

func (r *MachineTagResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*goztl.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected Resource Configure Type", "Expected a Zentral API client. Please report this issue to the provider developers.")
		return
	}
	r.client = client
}

func machineTagID(serial string, tagID int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(serial)) + "/" + strconv.FormatInt(tagID, 10)
}

func (r *MachineTagResource) membership(ctx context.Context, data machineTag) (bool, int, error) {
	var machine struct {
		SerialNumber string      `json:"serial_number"`
		Tags         []goztl.Tag `json:"tags"`
	}
	// A leading dot tells Zentral to decode the serial, including slashes and leading dots.
	serial := base64.RawURLEncoding.EncodeToString([]byte(data.SerialNumber.ValueString()))
	status, err := deviceRequest(ctx, r.client, http.MethodGet, "inventory/machines/."+serial+"/meta/", nil, &machine)
	if err != nil {
		return false, status, err
	}
	if machine.SerialNumber != data.SerialNumber.ValueString() || machine.Tags == nil {
		return false, status, fmt.Errorf("inventory response did not contain the requested machine and its tags")
	}
	for _, tag := range machine.Tags {
		if int64(tag.ID) == data.TagID.ValueInt64() {
			return true, status, nil
		}
	}
	return false, status, nil
}

func (r *MachineTagResource) mutate(ctx context.Context, data machineTag, kind string) error {
	tag, response, err := r.client.Tags.GetByID(ctx, int(data.TagID.ValueInt64()))
	if err != nil {
		if response != nil && response.StatusCode == http.StatusNotFound && kind == "REMOVE" {
			return nil
		}
		if response != nil {
			return fmt.Errorf("could not resolve the inventory tag (HTTP %d)", response.StatusCode)
		}
		return fmt.Errorf("could not resolve the inventory tag")
	}
	if tag == nil || int64(tag.ID) != data.TagID.ValueInt64() || tag.Name == "" {
		return fmt.Errorf("inventory response did not contain the requested tag")
	}
	// Names are globally unique. Omitting taxonomy preserves the existing tag's taxonomy.
	body := machineTagRequest{
		SerialNumbers: []string{data.SerialNumber.ValueString()},
		Operations:    []machineTagOperation{{Kind: kind, Names: []string{tag.Name}}},
	}
	_, err = deviceRequest(ctx, r.client, http.MethodPost, "inventory/machines/tags/", body, nil)
	return err
}

func (r *MachineTagResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data machineTag
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	present, _, err := r.membership(ctx, data)
	if err != nil {
		resp.Diagnostics.AddError("Unable to read machine tag", err.Error())
		return
	}
	if !present {
		if err := r.mutate(ctx, data, "ADD"); err != nil {
			resp.Diagnostics.AddError("Unable to add machine tag", err.Error())
			return
		}
	}
	data.ID = types.StringValue(machineTagID(data.SerialNumber.ValueString(), data.TagID.ValueInt64()))
	// Retain ownership after a successful mutation even if the confirmation read fails.
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
	present, _, err = r.membership(ctx, data)
	if err != nil {
		resp.Diagnostics.AddError("Unable to confirm machine tag", err.Error())
	} else if !present {
		resp.Diagnostics.AddError("Machine tag was not applied", "The requested direct tag association is absent after the update. Check for concurrent tag changes and retry.")
	}
}

func (r *MachineTagResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data machineTag
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	present, status, err := r.membership(ctx, data)
	if status == http.StatusNotFound {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to read machine tag", err.Error())
		return
	}
	if !present {
		resp.State.RemoveResource(ctx)
	}
}

func (r *MachineTagResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("Unexpected machine tag update", "A machine tag association has no mutable attributes. Identity changes must replace the resource.")
}

func (r *MachineTagResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data machineTag
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Remove by serial even after the inventory snapshot disappears; associations can outlive snapshots.
	if err := r.mutate(ctx, data, "REMOVE"); err != nil {
		resp.Diagnostics.AddError("Unable to remove machine tag", err.Error())
		return
	}
	present, status, err := r.membership(ctx, data)
	if status == http.StatusNotFound {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to confirm machine tag removal", err.Error())
	} else if present {
		resp.Diagnostics.AddError("Machine tag was not removed", "The direct tag association is still present after removal. Check for concurrent tag changes and retry.")
	}
}

func (r *MachineTagResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, "/")
	if len(parts) != 2 {
		resp.Diagnostics.AddError("Invalid machine tag import ID", "Expected unpadded base64url(serial_number)/tag_id.")
		return
	}
	serial, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	tagID, idErr := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || len(serial) == 0 || !utf8.Valid(serial) || idErr != nil || tagID < 1 || req.ID != machineTagID(string(serial), tagID) {
		resp.Diagnostics.AddError("Invalid machine tag import ID", "Expected a nonempty UTF-8 serial encoded with unpadded base64url, a slash, and a positive decimal tag ID.")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("serial_number"), string(serial))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("tag_id"), tagID)...)
}
