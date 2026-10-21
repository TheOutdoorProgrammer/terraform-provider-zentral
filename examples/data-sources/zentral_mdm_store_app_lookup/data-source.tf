data "zentral_mdm_store_app_lookup" "wireguard" {
  location_name = "Zentral"
  bundle_id     = "com.wireguard.ios"
}

output "wireguard_location_asset_id" {
  value = data.zentral_mdm_store_app_lookup.wireguard.id
}
