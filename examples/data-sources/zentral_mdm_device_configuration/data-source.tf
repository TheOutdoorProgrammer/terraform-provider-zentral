data "zentral_mdm_device_configuration" "vpn" {
  serial_number = "EXAMPLE-SERIAL"
  app_name      = "WireGuard"
  allow_missing = true
}

output "prepared" {
  value = data.zentral_mdm_device_configuration.vpn.found
}
