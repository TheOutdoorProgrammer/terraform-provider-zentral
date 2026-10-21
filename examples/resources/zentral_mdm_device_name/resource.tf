resource "zentral_mdm_device_name" "device" {
  serial_number      = "EXAMPLE123"
  enrolled_device_id = 42
  name               = "Example's iPad"
  timeout_seconds    = 120
}
