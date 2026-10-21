package provider

import (
	"context"
	"net/url"

	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/zentralopensource/goztl"
)

type MDMDeviceConfigurationDataSource struct{ client *goztl.Client }

type mdmDeviceConfigurationLookup struct {
	Serial       types.String `tfsdk:"serial_number"`
	AppName      types.String `tfsdk:"app_name"`
	AllowMissing types.Bool   `tfsdk:"allow_missing"`
	Found        types.Bool   `tfsdk:"found"`
	AppEligible  types.Bool   `tfsdk:"app_eligible"`
	ArtifactID   types.String `tfsdk:"artifact_id"`
	AttachmentID types.Int64  `tfsdk:"attachment_id"`
	TagID        types.Int64  `tfsdk:"tag_id"`
}

func NewMDMDeviceConfigurationDataSource() datasource.DataSource {
	return &MDMDeviceConfigurationDataSource{}
}

func (d *MDMDeviceConfigurationDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_mdm_device_configuration"
}

func (d *MDMDeviceConfigurationDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Finds an existing per-device profile requiring an app by exact app artifact name and full device serial. Requires the fork's nonsecret device configuration metadata endpoint. It verifies exclusive native tag/version/attachment scope without fetching profile source. It does not upload or activate a profile. The independent app_eligible result evaluates app-version platform, OS and tag scope, not blueprint assignment, signing validity, licensing, installation or device reachability.",
		Attributes: map[string]schema.Attribute{
			"serial_number": schema.StringAttribute{Required: true, Description: "Exact full device serial number.", Validators: []validator.String{stringvalidator.LengthAtLeast(1)}},
			"app_name":      schema.StringAttribute{Required: true, Description: "Exact stable name of the required app artifact.", Validators: []validator.String{stringvalidator.LengthBetween(1, 256)}},
			"allow_missing": schema.BoolAttribute{Optional: true, Description: "Defaults to false. Allow an absent enrollment, app or per-device configuration to return found=false. Unsafe scope and API failures remain errors. Set true when using app_eligible without a profile."},
			"found":         schema.BoolAttribute{Computed: true, Description: "Whether one exclusively scoped per-device configuration was found."},
			"app_eligible":  schema.BoolAttribute{Computed: true, Description: "Whether an app version's native platform, OS and tag scope includes the device, independent of blueprint assignment."},
			"artifact_id":   schema.StringAttribute{Computed: true, Description: "Prepared profile artifact UUID, null when absent."},
			"attachment_id": schema.Int64Attribute{Computed: true, Description: "Attachment ID for the device's current blueprint, null when detached or absent."},
			"tag_id":        schema.Int64Attribute{Computed: true, Description: "Verified exclusive device tag ID, null when no configuration was found."},
		},
	}
}

func (d *MDMDeviceConfigurationDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *MDMDeviceConfigurationDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data mdmDeviceConfigurationLookup
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var result struct {
		Found        *bool   `json:"found"`
		AppEligible  *bool   `json:"app_eligible"`
		ArtifactID   *string `json:"artifact_id"`
		AttachmentID *int64  `json:"attachment_id"`
		TagID        *int64  `json:"tag_id"`
	}
	query := url.Values{"serial_number": {data.Serial.ValueString()}, "app_name": {data.AppName.ValueString()}}
	if _, err := deviceRequest(ctx, d.client, "GET", "mdm/devices/configuration_target/?"+query.Encode(), nil, &result); err != nil {
		resp.Diagnostics.AddError("Device configuration lookup failed", err.Error()+". This lookup requires the nonsecret configuration metadata endpoint in Zentral.")
		return
	}
	if result.Found == nil || result.AppEligible == nil {
		resp.Diagnostics.AddError("Invalid configuration metadata", "Zentral did not return the required configuration lookup fields.")
		return
	}
	if *result.Found && (result.ArtifactID == nil || *result.ArtifactID == "" || result.TagID == nil || *result.TagID <= 0) ||
		!*result.Found && (result.ArtifactID != nil || result.TagID != nil || result.AttachmentID != nil) ||
		result.AttachmentID != nil && *result.AttachmentID <= 0 {
		resp.Diagnostics.AddError("Invalid configuration metadata", "Zentral returned inconsistent configuration identity metadata.")
		return
	}
	if result.ArtifactID != nil {
		if _, err := uuid.Parse(*result.ArtifactID); err != nil {
			resp.Diagnostics.AddError("Invalid configuration metadata", "Zentral returned an invalid artifact identifier.")
			return
		}
	}
	if !*result.Found && !data.AllowMissing.ValueBool() {
		resp.Diagnostics.AddError("Device configuration not found", "No exclusive per-device configuration was found. Prepare the native configuration or set allow_missing=true to stage this device.")
		return
	}
	data.Found = types.BoolValue(*result.Found)
	data.AppEligible = types.BoolValue(*result.AppEligible)
	data.ArtifactID = types.StringPointerValue(result.ArtifactID)
	data.AttachmentID = types.Int64PointerValue(result.AttachmentID)
	data.TagID = types.Int64PointerValue(result.TagID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
