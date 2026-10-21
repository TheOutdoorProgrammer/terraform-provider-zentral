data "zentral_mdm_enrolled_device" "tablet" {
  serial_number = "EXAMPLE-SERIAL"
  allow_missing = true
}

output "enrolled" {
  value = data.zentral_mdm_enrolled_device.tablet.found
}
