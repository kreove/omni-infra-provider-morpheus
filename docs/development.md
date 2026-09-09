# Development

## Prerequisites

- Go, at the version in `go.mod`
- `protoc`, `protoc-gen-go` and `protoc-gen-go-vtproto`, only to regenerate `api/specs`
- Docker, to build the container

## Building and testing

```bash
make build
make test
make vet
make fmt
```

### The Go 1.27 build failure

On a Go 1.27 toolchain the build fails with:

```
undefined: http2.TrailerPrefix
```

This is not a fault in this project. Go 1.27 changed how `golang.org/x/net/http2` is built, and `x/net` now excludes the file defining that constant under `//go:build !(go1.27 && !http2legacy)`. gRPC 1.81, pulled in transitively by the Omni client, still expects it.

Build with the version the module targets:

```bash
GOTOOLCHAIN=go1.26.7 make test
```

`GOTOOLCHAIN=auto` does not help: it only ever selects a *newer* toolchain than the installed one, never an older one. CI and the Dockerfile both pin the toolchain, so neither is affected.

The fix, when it arrives, is a dependency bump rather than a change here.

## Regenerating protobuf

`api/specs` is generated. After editing `specs.proto`:

```bash
make generate
```

## Layout

| Path | Contents |
| --- | --- |
| `cmd/omni-infra-provider-morpheus` | entrypoint, flags, Omni registration |
| `internal/pkg/provider/client.go` | HTTP transport, auth, error envelopes |
| `internal/pkg/provider/api.go` | typed Morpheus endpoints |
| `internal/pkg/provider/resolve.go` | Machine Class refs to Morpheus IDs |
| `internal/pkg/provider/image.go` | download, decompress, upload, cache |
| `internal/pkg/provider/provision.go` | provisioning steps, deprovisioning |
| `internal/pkg/provider/data` | Machine Class schema |
| `api/specs` | generated protobuf state |

`data.Data` and `data/schema.json` describe the same thing to different audiences — Go and Omni's form renderer. **They must be changed together**; there is nothing that enforces it.

## Testing approach

Everything is unit tested. There is no live-appliance test, which is the main gap relative to the sibling providers — see [Compatibility](compatibility.md).

Two areas carry most of the weight:

- **`client_test.go`** runs against an `httptest` server standing in for Morpheus, covering auth (both modes), token caching, 401 recovery, the `success: false` envelope, and exact-name filtering.
- **`provision_test.go`** asserts the shape of the provisioning payload field by field. Since the payload cannot be validated against a real appliance, these tests are what pins it to the contract derived from the SDK, the Terraform provider and the API reference. If you change the payload, change these deliberately — they are the specification.

When adding a test, prefer asserting on behaviour that would break something real. `TestListInstancesByNameFiltersToExactMatches` exists because substring matching silently stops a cluster scaling past nine nodes; the comment says so, so a later reader does not delete it as redundant.

## Adding support for another Morpheus cloud type

The provider targets MVM only. The cloud-specific part is the `config` block in `buildInstancePayload`, plus `mvmProviderType` in the resource-pool filter.

vSphere via Morpheus is the smallest step: it uses `config.template` rather than `config.imageId` for the virtual image, adds `vmwareFolderId`, and wants vmdk/ovf images rather than qcow2. That means branching the config block on a cloud-type field in the Machine Class, and mapping `image_format` to the artifacts the Image Factory publishes for that platform.

## Conventions

Comments explain *why*, not *what* — particularly where the code looks odd. Several deliberate-looking oddities here are load-bearing (exact-name filtering, the `network-<id>` string form, the detached import goroutine, checking `success` on a 200). If you remove one, remove its comment too; if you keep it, keep the reason.
