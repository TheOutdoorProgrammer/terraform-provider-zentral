package provider

import (
	"context"
	"errors"
	"net/url"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/zentralopensource/goztl"
)

var _ datasource.DataSource = &MDMEnrolledDeviceDataSource{}

type MDMEnrolledDeviceDataSource struct {
	client *goztl.Client
}

type mdmEnrolledDeviceLookup struct {
	Serial       types.String `tfsdk:"serial_number"`
	AllowMissing types.Bool   `tfsdk:"allow_missing"`
	Found        types.Bool   `tfsdk:"found"`
	ID           types.Int64  `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	Platform     types.String `tfsdk:"platform"`
	Supervised   types.Bool   `tfsdk:"supervised"`
	BlueprintID  types.Int64  `tfsdk:"blueprint_id"`
}

type enrolledDeviceLookupResult struct {
	namedDevice
	BlueprintID *int64 `json:"blueprint"`
}

func NewMDMEnrolledDeviceDataSource() datasource.DataSource {
	return &MDMEnrolledDeviceDataSource{}
}

func (d *MDMEnrolledDeviceDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_mdm_enrolled_device"
}

func (d *MDMEnrolledDeviceDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Finds one active, unblocked enrollment by its exact full serial number. Checked-out and blocked enrollments are excluded. Ambiguous active identities fail rather than selecting a device by enrollment order. This read-only lookup never sends a device command. Serial numbers and device names are stored in state; protect state as personal data.",
		Attributes: map[string]schema.Attribute{
			"serial_number": schema.StringAttribute{Required: true, Description: "Exact full device serial number.", Validators: []validator.String{stringvalidator.LengthAtLeast(1)}},
			"allow_missing": schema.BoolAttribute{Optional: true, Description: "Allow a device that has not enrolled yet. Defaults to false. When true, a missing device returns found=false and null computed identity fields. API failures and ambiguous identities remain errors."},
			"found":         schema.BoolAttribute{Computed: true, Description: "Whether exactly one active, unblocked enrollment was found."},
			"id":            schema.Int64Attribute{Computed: true, Description: "Zentral enrollment ID, or null when no eligible device was found."},
			"name":          schema.StringAttribute{Computed: true, Description: "Cached inventory device name. Custom DeviceInformation commands do not refresh this value."},
			"platform":      schema.StringAttribute{Computed: true, Description: "Device platform reported by Zentral."},
			"supervised":    schema.BoolAttribute{Computed: true, Description: "Whether Zentral records the device as supervised."},
			"blueprint_id":  schema.Int64Attribute{Computed: true, Description: "Current blueprint ID, or null when none is assigned."},
		},
	}
}

func (d *MDMEnrolledDeviceDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*goztl.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider client", "Expected a Zentral API client.")
		return
	}
	d.client = client
}

// Reconstruct pagination locally instead of following server-provided URLs with credentials.
func lookupEnrolledDevice(ctx context.Context, client *goztl.Client, serial string) (*enrolledDeviceLookupResult, error) {
	var found *enrolledDeviceLookupResult
	seen := make(map[int64]bool)
	count := -1
	for offset := 0; offset < 100000; {
		var page struct {
			Count   int                          `json:"count"`
			Results []enrolledDeviceLookupResult `json:"results"`
		}
		query := url.Values{"serial_number": {serial}, "limit": {"1000"}, "offset": {strconv.Itoa(offset)}}
		if _, err := deviceRequest(ctx, client, "GET", "mdm/devices/?"+query.Encode(), nil, &page); err != nil {
			return nil, err
		}
		if page.Count < 0 || (count >= 0 && page.Count != count) || offset+len(page.Results) > page.Count {
			return nil, errors.New("device inventory changed or returned inconsistent pagination; retry the lookup")
		}
		count = page.Count
		for _, item := range page.Results {
			if item.ID <= 0 || seen[item.ID] {
				return nil, errors.New("device inventory returned an invalid or repeated enrollment; retry the lookup")
			}
			seen[item.ID] = true
			if item.Serial != serial || item.Checkout != nil || item.Blocked != nil {
				continue
			}
			if found != nil {
				return nil, errors.New("multiple active enrollments match the exact serial number; resolve the ambiguity in Zentral")
			}
			copy := item
			found = &copy
		}
		offset += len(page.Results)
		if offset == count {
			return found, nil
		}
		if len(page.Results) == 0 {
			return nil, errors.New("device inventory pagination ended before all enrollments were checked")
		}
	}
	return nil, errors.New("device inventory exceeded the safe lookup size")
}

func (d *MDMEnrolledDeviceDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data mdmEnrolledDeviceLookup
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	device, err := lookupEnrolledDevice(ctx, d.client, data.Serial.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Device lookup failed", err.Error())
		return
	}
	data.Found = types.BoolValue(device != nil)
	data.ID = types.Int64Null()
	data.Name = types.StringNull()
	data.Platform = types.StringNull()
	data.Supervised = types.BoolNull()
	data.BlueprintID = types.Int64Null()
	if device == nil {
		if !data.AllowMissing.ValueBool() {
			resp.Diagnostics.AddError("Device not enrolled", "No active, unblocked enrollment matches the exact serial number. Enroll the device or set allow_missing=true to stage its configuration.")
			return
		}
	} else {
		data.ID = types.Int64Value(device.ID)
		data.Name = types.StringValue(device.Name)
		data.Platform = types.StringValue(device.Platform)
		data.Supervised = types.BoolValue(device.Supervised)
		if device.BlueprintID != nil {
			data.BlueprintID = types.Int64Value(*device.BlueprintID)
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
