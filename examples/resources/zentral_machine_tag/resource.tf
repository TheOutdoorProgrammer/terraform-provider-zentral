resource "zentral_tag" "device" {
  name = "example-ipad"
}

resource "zentral_machine_tag" "device" {
  serial_number = "EXAMPLE123"
  tag_id        = zentral_tag.device.id
}
