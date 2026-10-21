# Terraform Provider for Zentral

The Terraform Zentral provider is a plugin for Terraform that allows for the full lifecycle management of [Zentral](https://github.com/zentralopensource/zentral/) resources.

This fork adds [direct machine tag membership](docs/resources/machine_tag.md) and [MDM device names](docs/resources/mdm_device_name.md) so enrolled devices can be configured through OpenTofu without writing through data sources. It retains the upstream MPL-2.0 license. The resources use existing Zentral APIs; unattended naming also requires the companion server change that sends APNs notifications after custom-command creation.

See [the ownership and reconciliation decision](adr/0001-manage-device-identity-through-resources-with-read-only-refr.md) for lifecycle limits, timeout recovery, and why WireGuard profile contents stay outside state.

## Try a development build locally

Build a local binary:

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

The [device onboarding example](examples/device-onboarding/main.tf) declares a name and dedicated tag membership. With the development override configured, use `tofu plan` directly; provider installation during `tofu init` still requires a published registry version. Production configurations must use the actual private registry address and a pinned, verified release rather than this development address.

Import existing tags and associations before adopting them. The example does not upload a VPN profile or attach it to a blueprint. Private profile source remains in the confidential upload workflow; both version and blueprint scopes must independently restrict activation to the intended device.

## Release and private registry publication

Pushing a `v*` version tag runs the existing GoReleaser workflow. It signs SHA-256 checksums with the dedicated release key, publishes the archives to [GitHub Releases](https://github.com/TheOutdoorProgrammer/terraform-provider-zentral/releases), and retains the complete `dist` directory as the `provider-registry-<tag>` workflow artifact for 14 days. The private registry import uses those exact signed files without rebuilding them.

The workflow reads `GPG_PRIVATE_KEY` and `GPG_PRIVATE_KEY_PASSPHRASE` from repository secrets. Preserve the encrypted key and its passphrase in your secret manager before configuring them. Register only the public key with the destination registry. The key fingerprint must match both the detached checksum signature and the registry signing key.

The [release public key](release-signing-key.asc) has fingerprint `0DC7B0E7F52E7B980C77A22E3037EE62A296FE95` and expires on October 20, 2028. Verify the fingerprint against a trusted checkout before importing it for checksum verification. Key rotation requires updating both registry trust and this public key before publishing with its replacement.

From a trusted host that can reach the private registry, authenticate `spacectl`, download the workflow artifact with `gh run download`, verify its checksum signature against the pinned public key, and import it:

```shell
spacectl provider create-version --type zentral --protocols 6.0 \
  --goreleaser-dir /absolute/path/to/downloaded/dist
spacectl provider list-versions --type zentral
spacectl provider publish-version --version <verified-draft-version-id>
```

Only publish after successful verification. The provider source is `<registry-host>/<account>/zentral`; use the address returned by your registry. Test installation for every platform used by operators and runners, commit the resulting dependency lock file, and verify a refresh-only or no-change plan before migrating an existing stack. Provider source migration also requires replacing the old provider address in state under the stack's normal locking procedure. Neither installing the provider nor changing its source automatically adopts device resources.

The registry is deliberately private. GitHub-hosted runners cannot reach a LAN-only registry, so release publication and registry promotion are separate steps. Preserve registry upload errors in restricted logs because they may contain presigned storage URLs.

## Telemetry

Telemetry is disabled unless an operator explicitly configures an OTLP endpoint. `OTEL_EXPORTER_OTLP_ENDPOINT` enables both traces and correlated request logs using OTLP HTTP/protobuf; the signal-specific `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` and `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` settings enable their respective signals. Supply ingestion headers through protected runtime environment settings, never HCL, command arguments, or committed files. Request logs do not require `TF_LOG`.

Log export uses system TLS trust and proxy settings with a five-second request timeout. OTLP certificate-file and mutual-TLS environment options are not supported by the log transport. Configure a trusted collector endpoint with system trust or an appropriate private network transport.

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
