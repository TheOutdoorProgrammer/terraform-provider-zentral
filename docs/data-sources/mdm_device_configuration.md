---
page_title: "zentral_mdm_device_configuration Data Source - zentral"
subcategory: ""
description: |-
  Resolve nonsecret device configuration targeting metadata.
---

# zentral_mdm_device_configuration (Data Source)

Finds a prepared per-device profile by exact full device serial and the stable name of an app artifact it requires. This data source needs the Zentral fork's `GET /api/mdm/devices/configuration_target/` endpoint. Older servers fail explicitly. It never fetches a profile source, VPN key, app package or provisioning profile.

```terraform
data "zentral_mdm_device_configuration" "vpn" {
  serial_number = "EXAMPLE-SERIAL"
  app_name      = "WireGuard"
  allow_missing = true
}
```

The server validates a unique active enrollment, native platform/OS/tag version targeting, one exclusive device tag, and compatible attachment scope. Every version with a nonempty rollout must remain exclusive to this device, including historical versions and versions that target other platforms. A broadly targeted configuration, shared device tag, or ambiguous matching configuration causes an error even when `allow_missing` is true. A detached configuration or disabled attachment remains discoverable so it can be assigned again. `attachment_id` identifies only the device's current blueprint attachment and is null when detached. Configuration discovery requires the device to have an assigned blueprint.

`app_eligible` is independent of configuration discovery and blueprint assignment. It reports whether native app-version platform, OS and tag filters include the device. It does not prove app signing eligibility, provisioning validity, available licenses, installation, or device connectivity. Set `allow_missing = true` when using this result for an app that has no per-device profile.

This read-only lookup does not upload or activate a profile. Credential-bearing source must remain outside Terraform state. Serial numbers are stored in state and must be protected as personal data.

The API account requires `mdm.view_enrolleddevice`, `mdm.view_artifact`, `mdm.view_blueprintartifact`, `inventory.view_machinesnapshot`, and `inventory.view_tag`.

## Schema

### Required

- `serial_number` (String) Exact full device serial number.
- `app_name` (String) Exact required app artifact name.

### Optional

- `allow_missing` (Boolean) Permit an absent device, app or configuration. Defaults to false.

### Read-Only

- `found` (Boolean) Whether one exclusively scoped configuration exists.
- `app_eligible` (Boolean) Native app-version platform, OS and tag applicability, independent of blueprint assignment.
- `artifact_id` (String) Prepared profile artifact UUID, null when missing.
- `attachment_id` (Number) Current blueprint attachment ID, null when detached or missing.
- `tag_id` (Number) Verified exclusive tag ID, null when missing.
