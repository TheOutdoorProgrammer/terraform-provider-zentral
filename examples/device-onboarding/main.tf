terraform {
  required_providers {
    zentral = {
      source = "theoutdoorprogrammer/zentral"
    }
  }
}

provider "zentral" {}

variable "serial_number" {
  type        = string
  description = "Exact serial number of the already enrolled device. Stored in state."
}

variable "enrolled_device_id" {
  type        = number
  description = "Exact active enrollment ID from Zentral."
}

variable "device_name" {
  type = string
}

variable "vpn_tag_name" {
  type        = string
  description = "A unique inventory tag reserved for this device's VPN."
}

resource "zentral_tag" "vpn" {
  name = var.vpn_tag_name
}

resource "zentral_machine_tag" "vpn" {
  serial_number = var.serial_number
  tag_id        = zentral_tag.vpn.id
}

resource "zentral_mdm_device_name" "device" {
  serial_number      = var.serial_number
  enrolled_device_id = var.enrolled_device_id
  name               = var.device_name
  timeout_seconds    = 120
}
