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

var _ datasource.DataSource = &MDMStoreAppLookupDataSource{}

type MDMStoreAppLookupDataSource struct {
	client *goztl.Client
}

type mdmStoreAppLookup struct {
	ID           types.Int64  `tfsdk:"id"`
	LocationName types.String `tfsdk:"location_name"`
	LocationID   types.Int64  `tfsdk:"location_id"`
	AdamID       types.String `tfsdk:"adam_id"`
	PricingParam types.String `tfsdk:"pricing_param"`
	Name         types.String `tfsdk:"name"`
	BundleID     types.String `tfsdk:"bundle_id"`
}

type storeAppLookupAsset struct {
	ID               int64   `json:"id"`
	LocationID       int64   `json:"location"`
	AdamID           string  `json:"adam_id"`
	PricingParam     string  `json:"pricing_param"`
	Name             *string `json:"name"`
	BundleID         *string `json:"bundle_id"`
	ProductType      *string `json:"product_type"`
	DeviceAssignable *bool   `json:"device_assignable"`
	TotalCount       *int64  `json:"total_count"`
	RetiredCount     *int64  `json:"retired_count"`
}

func NewMDMStoreAppLookupDataSource() datasource.DataSource {
	return &MDMStoreAppLookupDataSource{}
}

func (d *MDMStoreAppLookupDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_mdm_store_app_lookup"
}

func (d *MDMStoreAppLookupDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Resolves a device-assignable Apps and Books app by exact location name and bundle ID. Requires the companion Zentral location-asset metadata API extension. Missing or ambiguous matches fail; this data source never acquires licenses or creates assets.",
		Attributes: map[string]schema.Attribute{
			"id":            schema.Int64Attribute{Computed: true, Description: "Location-asset ID for use in zentral_mdm_store_app.location_asset_id."},
			"location_name": schema.StringAttribute{Required: true, Description: "Exact Apps and Books location name.", Validators: []validator.String{stringvalidator.LengthAtLeast(1)}},
			"bundle_id":     schema.StringAttribute{Required: true, Description: "Exact app bundle identifier, for example com.wireguard.ios.", Validators: []validator.String{stringvalidator.LengthAtLeast(1)}},
			"location_id":   schema.Int64Attribute{Computed: true, Description: "Resolved Apps and Books location ID."},
			"adam_id":       schema.StringAttribute{Computed: true, Description: "Apple's App Store ID."},
			"pricing_param": schema.StringAttribute{Computed: true, Description: "Apple's pricing parameter for the resolved asset."},
			"name":          schema.StringAttribute{Computed: true, Description: "App name from synchronized Apple metadata."},
		},
	}
}

func (d *MDMStoreAppLookupDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *MDMStoreAppLookupDataSource) lookup(ctx context.Context, locationName, bundleID string) (*storeAppLookupAsset, error) {
	var locations struct {
		Count   *int64              `json:"count"`
		Next    *string             `json:"next"`
		Results []goztl.MDMLocation `json:"results"`
	}
	_, err := deviceRequest(ctx, d.client, "GET", "mdm/locations/?"+url.Values{"name": {locationName}, "limit": {"2"}}.Encode(), nil, &locations)
	if err != nil {
		return nil, err
	}
	if locations.Count == nil || *locations.Count < 0 {
		return nil, errors.New("invalid Apps and Books location list response")
	}
	if *locations.Count > 1 {
		return nil, errors.New("Apps and Books location name is ambiguous; use a unique location name")
	}
	if int64(len(locations.Results)) != *locations.Count || locations.Next != nil {
		return nil, errors.New("incomplete Apps and Books location list response")
	}
	if *locations.Count == 0 {
		return nil, errors.New("Apps and Books location was not found; check the exact location name and synchronization")
	}
	location := locations.Results[0]
	if location.ID < 1 || location.Name != locationName {
		return nil, errors.New("Apps and Books location response did not match the exact requested name")
	}
	var assets struct {
		Count   *int64                `json:"count"`
		Next    *string               `json:"next"`
		Results []storeAppLookupAsset `json:"results"`
	}
	_, err = deviceRequest(ctx, d.client, "GET", "mdm/location_assets/?"+url.Values{
		"location_id": {strconv.Itoa(location.ID)}, "bundle_id": {bundleID}, "limit": {"2"},
	}.Encode(), nil, &assets)
	if err != nil {
		return nil, err
	}
	if assets.Count == nil || *assets.Count < 0 {
		return nil, errors.New("invalid Apps and Books asset list response")
	}
	if *assets.Count > 1 {
		return nil, errors.New("multiple assets match the app bundle ID in this location; resolve duplicate pricing variants or use zentral_mdm_location_asset explicitly")
	}
	if int64(len(assets.Results)) != *assets.Count || assets.Next != nil {
		return nil, errors.New("incomplete Apps and Books asset list response")
	}
	if *assets.Count == 0 {
		return nil, errors.New("app was not found in this Apps and Books location; acquire licenses and synchronize app metadata in Zentral")
	}
	asset := assets.Results[0]
	if asset.BundleID == nil || asset.Name == nil || asset.ProductType == nil || asset.DeviceAssignable == nil {
		return nil, errors.New("asset metadata is unavailable; deploy the companion Zentral location-asset metadata API extension and synchronize Apple metadata")
	}
	if asset.ID < 1 || asset.LocationID != int64(location.ID) || *asset.BundleID != bundleID || asset.AdamID == "" || asset.PricingParam == "" || *asset.Name == "" {
		return nil, errors.New("asset response did not contain the exact requested app and location")
	}
	if *asset.ProductType != "App" || !*asset.DeviceAssignable {
		return nil, errors.New("the matching asset is not a device-assignable app")
	}
	if asset.TotalCount == nil || asset.RetiredCount == nil || *asset.RetiredCount < 0 || *asset.TotalCount <= *asset.RetiredCount {
		return nil, errors.New("the matching app has no active licenses; acquire licenses and synchronize this location")
	}
	return &asset, nil
}

func (d *MDMStoreAppLookupDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data mdmStoreAppLookup
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if data.LocationName.IsNull() || data.LocationName.IsUnknown() || data.BundleID.IsNull() || data.BundleID.IsUnknown() {
		resp.Diagnostics.AddError("App lookup inputs unavailable", "The location name and bundle ID must be known before resolving an app.")
		return
	}
	asset, err := d.lookup(ctx, data.LocationName.ValueString(), data.BundleID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("App lookup failed", err.Error())
		return
	}
	data.ID = types.Int64Value(asset.ID)
	data.LocationID = types.Int64Value(asset.LocationID)
	data.AdamID = types.StringValue(asset.AdamID)
	data.PricingParam = types.StringValue(asset.PricingParam)
	data.Name = types.StringValue(*asset.Name)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
