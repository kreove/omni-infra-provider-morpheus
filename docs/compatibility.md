# Compatibility and limitations

## Status

This provider is **alpha and unvalidated against a live Morpheus appliance**. The VergeOS and Xen Orchestra providers it was ported from were both exercised end-to-end against real infrastructure before release. This one has not been.

What it *is* built on:

- The official [Morpheus Go SDK](https://github.com/gomorpheus/morpheus-go-sdk), for endpoint paths, response envelopes and JSON keys
- The official [Morpheus Terraform provider](https://github.com/gomorpheus/terraform-provider-morpheus), specifically `resource_mvm_instance.go`, for the MVM provisioning payload
- The [Morpheus API reference](https://apidocs.morpheusdata.com/reference/addinstance), for the `config` block and its per-cloud variants
- The [Morpheus CLI](https://github.com/gomorpheus/morpheus-cli), for `servicePlanOptions` semantics and units

Everything the provider sends is unit-tested for shape. None of it has been acknowledged by a real appliance.

## Known-uncertain areas

These are the places most likely to need adjustment on first contact, roughly in order of risk.

### 1. Cloud-init user data passthrough

**This is the highest-risk part of the port.**

Talos does not consume cloud-config YAML. Its NoCloud datasource reads user-data and expects a Talos machine configuration — here, the join config Omni supplies. Anything Morpheus adds to, merges into, or reformats in that user-data is read by Talos as part of its machine config, and the node fails to join.

The provider sends the join config as `config.userData`, which the API reference documents as *"allows for override of cloud-init based user-data yaml or custom scripts"*, and disables the two features known to rewrite user data:

- `createUser: false` — Morpheus otherwise injects a login user into cloud-init. Talos has no user accounts.
- `noAgent: true` — the Morpheus agent is a package installed into the guest. Talos has no package manager and no shell.

If nodes boot but sit in maintenance mode without joining, this is the first thing to check: pull the instance's rendered cloud-init user data from Morpheus and confirm it is byte-identical to the join config. See [Troubleshooting](troubleshooting.md#nodes-boot-but-never-join-omni).

The Xen Orchestra provider hit exactly this class of problem — XO built a config drive whose *contents* were correct but whose *layout* Talos could not detect — and had to build the drive itself. If Morpheus turns out to mangle user data in a way that cannot be disabled, the equivalent fix here is heavier: Morpheus owns config-drive generation, so there is no obvious injection point.

### 2. Instance type and layout defaults

`instance_type_code` defaults to `vm`, which is Morpheus's built-in type for provisioning a plain VM from an image. If that code does not exist on your appliance, the provider fails at `ensureTarget` with a list of the instance types that *do* exist — set `instance_type` or `instance_type_code` from that list.

Layouts are not defaulted, because a layout determines the provision type and only an MVM layout will work here.

### 3. Sizing overrides

`cores`, `memory` and `disk_size` are sent as `servicePlanOptions` and a root volume, and are only honoured on a service plan that permits custom sizing. On a fixed plan they are ignored. This differs from the VergeOS and Xen Orchestra providers, where those fields were authoritative. See [Configuration](configuration.md#sizing-is-the-service-plans-job).

Memory is stated in MiB in the Machine Class and converted to bytes, which is what Morpheus expects in this field.

### 4. Image readiness

After upload, Morpheus processes an image asynchronously. The provider polls the virtual image until it reports an active status. Morpheus spells that status differently across versions and cloud types, and some versions leave it empty on a completed upload — so a non-empty size is accepted as evidence on its own. If imports hang at "waiting", check what your appliance actually reports in `status`.

### 5. Boot firmware

The Talos nocloud image boots under both BIOS and UEFI, so `uefi` is left unset by default and the platform default applies. If VMs power on and immediately halt without console output, set `uefi: true` (or `false`) explicitly.

## Supported

| | |
| --- | --- |
| Architecture | `amd64` only |
| Morpheus cloud type | MVM (Morpheus VM Manager / HPE VM Essentials) only |
| Image formats | `qcow2` (default), `raw` |
| Authentication | API token, or username and password |
| Provider replicas | One |

## Not supported

- **`arm64`.** The Machine Class schema rejects it. Adding it is mostly a matter of removing the guard and testing.
- **Other Morpheus cloud types.** The `config` block is MVM-specific: `poolProviderType: "mvm"` and `imageId`. vSphere via Morpheus uses `config.template` for the image and adds `vmwareFolderId`; AWS, Azure and GCP differ further. Supporting them means branching the config block per cloud type.
- **Multiple provider replicas.** Nothing serializes instance creation between replicas. The image cache is keyed by a deterministic name and tolerates a race, but instance creation does not. Run one replica.
- **Multiple NICs**, static IP assignment, and instance resize after creation. Omni scales by adding and removing machines, so resize has not been needed.
- **Snapshots and backups.** Out of scope; manage them in Morpheus.

## Version compatibility

Developed against the Morpheus API as documented for 8.x. The endpoints used are long-standing and present well before that, but `/api/options/zonePools` and the MVM provision type require a Morpheus version that supports MVM clouds.
