---
page_title: "zentral_mdm_enrolled_device Data Source - zentral"
subcategory: ""
description: |-
  Find one active enrolled device by its exact serial number.
---

# zentral_mdm_enrolled_device (Data Source)

Resolves the current enrollment ID by exact full serial number. Historical checked-out and blocked enrollments are excluded. Ambiguous active matches cause an error. The lookup reads all result pages and never sends device commands.

```terraform
data "zentral_mdm_enrolled_device" "tablet" {
  serial_number = "EXAMPLE-SERIAL"
  allow_missing = true
}
```

`allow_missing` stages devices that have not enrolled yet. Missing devices return `found = false` and null computed identity fields. Permission failures, inconsistent pagination and ambiguity remain errors. Re-enrollment produces a new ID, allowing dependent resources to detect a changed enrollment identity.

Serial numbers and names are stored in state. Protect state as personal data. The account requires `mdm.view_enrolleddevice`.

## Schema

### Required

- `serial_number` (String) Exact full device serial number.

### Optional

- `allow_missing` (Boolean) Permit no eligible enrollment. Defaults to false.

### Read-Only

- `id` (Number) Enrollment ID, null when missing.
- `found` (Boolean) Whether one eligible enrollment exists.
- `name` (String) Cached inventory name. Custom DeviceInformation verification does not update this cache.
- `platform` (String) Reported device platform.
- `supervised` (Boolean) Recorded supervision status.
- `blueprint_id` (Number) Current blueprint ID, null when missing or unassigned.
