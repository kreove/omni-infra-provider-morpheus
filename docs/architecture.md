# Architecture

## Shape

The provider is a single process that registers with Omni as an infrastructure provider and reconciles Machine Requests against a Morpheus appliance.

```
cmd/omni-infra-provider-morpheus   entrypoint, flags, Omni registration
internal/pkg/provider
  client.go       HTTP transport, authentication, error envelopes
  api.go          typed Morpheus endpoints
  resolve.go      Machine Class refs -> Morpheus IDs
  image.go        Image Factory download, decompression, upload, caching
  provision.go    provisioning steps and deprovisioning
  data/           Machine Class schema
  resources/      COSI resource stored in Omni
api/specs         protobuf state persisted per machine
```

## Provisioning steps

Omni drives the provider through the steps below. Each is idempotent and may be retried at any point; a step that needs to wait returns a retry interval rather than blocking.

1. **validateRequest** — length-checks the request ID and validates the Machine Class.
2. **ensureTarget** — resolves every Machine Class reference to a Morpheus ID. Runs before anything is created, so a bad Machine Class fails cleanly rather than halfway through provisioning.
3. **ensureImage** — asks Omni for the installation medium, which also ensures the schematic exists; then resolves a pinned image, or ensures the cached one exists, importing it if not. Kernel arguments are applied here. Retries while an import runs.
4. **syncMachine** — finds or creates the Morpheus instance, then polls until it is running.

There is no separate schematic step. Resolving the medium ensures the schematic and reports its ID in the same call, so a separate step would ask Omni for the same medium twice per reconcile — and the download URL it returns is short-lived, so it belongs in the step that fetches it.

Deprovisioning is separate: power off, then delete with volumes, retrying until the instance is gone.

## State

Per machine, the provider persists in Omni:

| Field | Purpose |
| --- | --- |
| `uuid` | Morpheus instance UUID |
| `instance_id` | Morpheus instance ID, used to deprovision |
| `schematic` | Image Factory schematic ID |
| `talos_version` | Talos version this machine was built for |
| `image_id` | Virtual image the machine was provisioned from |

`instance_id` is recorded at creation and preferred over the name when deprovisioning. A name lookup alone would strand an instance an operator had renamed in Morpheus — Omni would report the machine gone while the VM kept running.

## Image cache

Caching is keyed on the `StorageKey` Omni reports for the installation medium:

```
omni-talos-<StorageKey>
```

Two properties matter. The name is **stable**, so a restarted provider finds the image it already imported rather than re-importing it. And it is **distinct per medium**, so a machine cannot be built from the wrong Talos image. Both are covered by tests.

The download URL is deliberately *not* used to derive the name. It can carry credentials or a short-lived download token, so a name derived from it would change whenever those rotate and orphan the image already stored under the old name. `StorageKey` exists precisely for callers that store what they download, and changes only when the medium itself does.

For the same reason the URL is never logged and never written into the Morpheus image description.

The cache lives in Morpheus, not in the provider. The in-memory map only tracks *in-progress* imports, so that concurrent Machine Requests for the same image wait on one transfer instead of starting several.

Imports run detached from the request that triggered them. A multi-gigabyte download, decompression and upload would otherwise hold a single provisioning step open far past any sensible timeout; instead the step returns immediately and Omni retries it while the transfer proceeds.

An import that fails is dropped from the map so a later request retries it from scratch, and an incomplete virtual image is deleted from Morpheus — a record with no file behind it would satisfy the name lookup and silently provision every machine from an empty image.

## Client

The Morpheus client is written for this provider rather than taken from the official Go SDK. The SDK accepts no `context.Context` on any method, which an Omni provisioner needs — steps must be cancellable and deadline-bounded — and it returns results as `interface{}` requiring a type assertion at every call site.

Two behaviours in the client are worth knowing about:

- **Success is checked twice.** Morpheus reports some failures with HTTP 200 and `success: false`. Trusting the status code alone silently accepts failed operations, so both are checked, and per-field validation errors are included in the error message.
- **Name filters are narrowed.** Morpheus matches names by substring, so `talos-1` also returns `talos-10`. Results are filtered to exact matches. Without that, a cluster stops scaling correctly past nine nodes: the provider sees an existing instance where there is none.

Authentication is either a static API token or a username and password exchanged for an OAuth token. In the second case the token is cached, renewed ahead of expiry, and re-fetched once if Morpheus rejects it — an appliance restart otherwise breaks the provider until the process is restarted by hand.

## Talos delivery

Talos reads its machine configuration from the NoCloud datasource. The provider passes the Omni join config through as `config.userData` and disables the Morpheus features that would rewrite it — guest user creation and agent install. This is the most delicate part of the integration; see [Compatibility](compatibility.md#1-cloud-init-user-data-passthrough).
