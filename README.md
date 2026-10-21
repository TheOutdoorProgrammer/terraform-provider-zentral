# Terraform Provider for Zentral

The Terraform Zentral provider is a plugin for Terraform that allows for the full lifecycle management of [Zentral](https://github.com/zentralopensource/zentral/) resources.

This fork adds [direct machine tag membership](docs/resources/machine_tag.md) and [MDM device names](docs/resources/mdm_device_name.md) so enrolled devices can be configured through OpenTofu without writing through data sources. It retains the upstream MPL-2.0 license. The resources use existing Zentral APIs; unattended naming also requires the companion server change that sends APNs notifications after custom-command creation.

See [the ownership and reconciliation decision](adr/0001-manage-device-identity-through-resources-with-read-only-refr.md) for lifecycle limits, timeout recovery, and why WireGuard profile contents stay outside state.

## Try the unreleased fork locally

No registry release is implied by this checkout. Build a local binary:

```shell
mkdir -p dist/dev
go build -o dist/dev/terraform-provider-zentral .
```

Create a separate OpenTofu CLI configuration with the absolute build directory:

```hcl
provider_installation {
  dev_overrides {
    "registry.opentofu.org/theoutdoorprogrammer/zentral" = "/absolute/path/to/terraform-provider-zentral/dist/dev"
  }
  direct {}
}
```

Use `TF_CLI_CONFIG_FILE` to select that file only for the development command. Use `registry.terraform.io` instead for Terraform. Set `ZTL_API_BASE_URL` and `ZTL_API_TOKEN` through your approved credential mechanism, outside HCL and command arguments. The provider base URL includes the API prefix, such as `https://mdm.example.com/api/`.

The [device onboarding example](examples/device-onboarding/main.tf) declares a name and dedicated tag membership. With the development override configured, use `tofu plan` directly; provider installation during `tofu init` still requires a published registry version. Do not change an existing production provider pin to this unreleased source. A signed release and installation/lock-file verification are separate rollout steps.

Import existing tags and associations before adopting them. The example does not upload a VPN profile or attach it to a blueprint. Private profile source remains in the confidential upload workflow; both version and blueprint scopes must independently restrict activation to the intended device.

## Telemetry

Tracing is disabled unless an operator explicitly configures `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`. The exporter uses OTLP HTTP/protobuf and the standard OTLP environment configuration. Supply ingestion headers through protected runtime environment settings, never HCL, command arguments, or committed files.

Requests propagate W3C TraceContext and Baggage. Exported spans contain only fixed service attributes, HTTP methods, allowlisted route templates, status codes and bounded error categories. Provider request logs include trace/span IDs. Bodies, tokens, headers, query strings, device names and serials are not exported. Local collector tests cover this boundary; production exporter verification is required before rollout.

## Local resource tests

The new tests use local fake APIs, not real managed devices. With OpenTofu installed:

```shell
TF_ACC_TERRAFORM_PATH="$(command -v tofu)" \
TF_ACC_PROVIDER_HOST=registry.opentofu.org \
TF_ACC_PROVIDER_NAMESPACE=hashicorp \
go test -race ./internal/provider -run 'Test(MachineTag|MDMDeviceName|DeviceRequest)'
go test -race ./internal/telemetry
```

The host and namespace variables let the SDK test harness install its local test provider using OpenTofu's address rules. They do not fetch a provider named `hashicorp/zentral`. Leave `TF_ACC` unset to avoid upstream tests against a real server.

## Requirements

- [Terraform](https://www.terraform.io/downloads.html) >= 1.0
- [Go](https://golang.org/doc/install) >= 1.26

## Building The Provider

1. Clone the repository
1. Enter the repository directory
1. Build the provider using the Go `install` command:

```shell
go install
```

## Adding Dependencies

This provider uses [Go modules](https://github.com/golang/go/wiki/Modules).
Please see the Go documentation for the most up to date information about using Go modules.

To add a new dependency `github.com/author/dependency` to your Terraform provider:

```shell
go get github.com/author/dependency
go mod tidy
```

Then commit the changes to `go.mod` and `go.sum`.

## Using the provider

TBD

## Developing the Provider

If you wish to work on the provider, you'll first need [Go](http://www.golang.org) installed on your machine (see [Requirements](#requirements) above).

To compile the provider, run `go install`. This will build the provider and put the provider binary in the `$GOPATH/bin` directory.

To generate or update documentation, run `go generate`.

In order to run the full suite of Acceptance tests, run `make testacc`.

*Note:* Acceptance tests create real resources, and often cost money to run.

```shell
make testacc
```
