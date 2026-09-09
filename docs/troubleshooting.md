# Troubleshooting

Start with the provider logs:

```bash
docker compose -f deploy/docker-compose.yml logs -f
```

```bash
kubectl -n omni-infra-provider-morpheus logs -f deploy/omni-infra-provider-morpheus
```

The provider logs one line per state change per machine, and returns errors to Omni, where they appear on the Machine Request.

## The provider will not start

### `OMNI_SERVICE_ACCOUNT_KEY is not valid base64`

The key picked up a stray character. The provider strips whitespace before decoding and reports which character position is at fault and why — read the message, it names the specific problem (a duplicated trailing `=` is the usual one).

The key is displayed only when the service account is created. If you no longer have a clean copy:

```bash
omnictl infraprovider renewkey morpheus
```

### `set MORPHEUS_TOKEN or both MORPHEUS_USERNAME and MORPHEUS_PASSWORD`

No Morpheus credentials were supplied. Note that an empty-string value counts as unset.

### `morpheus-endpoint is required`

`MORPHEUS_ENDPOINT` (or `MORPHEUS_URL`) is unset. A bare hostname is fine — `https://` is assumed.

## Authentication failures

### `authentication failed for user "..."`

Morpheus rejected the username and password. The appliance's own response is included in the message. Confirm the account can log in to the Morpheus UI, and that it is not an SSO-only account — the password grant needs a local password.

### Every call fails with HTTP 401

If using `MORPHEUS_TOKEN`, the token is wrong, expired, or revoked. Unlike a username/password session, a static token cannot be renewed by the provider — issue a new one under **user settings → API access**.

### Responses mention a missing `"zones"` field

```
Morpheus response for /api/zones has no "zones" field; check that the endpoint and credentials are correct
```

Morpheus answered with something other than the expected JSON — usually a login page, which some configurations return instead of a 401. Check that `MORPHEUS_ENDPOINT` points at the appliance root and not at a reverse proxy path that strips `/api`.

## Machine Class errors

### `<field> "<name>" does not exist in Morpheus; available: ...`

The name did not resolve. The error lists every object the provider could have matched, with IDs. Copy the right one into the Machine Class.

Note that lookups are **scoped**: layouts are filtered by the resolved instance type, and plans, networks and pools by the resolved layout and cloud. An object that exists on the appliance but is not offered for your cloud will not appear — which usually means the object is genuinely not usable there, not that the lookup is broken.

### `<field> "<name>" is ambiguous in Morpheus`

Morpheus does not enforce unique names and more than one object matched. Set an `id` instead; the provider refuses to guess, because picking the first match would make provisioning depend on listing order.

### `instance_type_code "vm" does not exist`

The default instance type code is not present on your appliance. Set `instance_type` or `instance_type_code` from the list in the error.

## Image import problems

### Imports never finish

The provider logs `starting Talos image import` and then machines sit at the `ensureImage` step. Image import runs detached from the request, so the step retries every 15 seconds while the transfer runs. A first import of a raw image can legitimately take a long time.

If it never completes:

- Check the provider has scratch space. A raw image decompresses to over a gigabyte; the container needs room in `/tmp`. Both deployment examples provision this.
- Check the provider can reach the Image Factory over HTTPS.
- Check the virtual image in Morpheus. If its status is stuck in a processing state, the upload landed but Morpheus could not convert it — try `image_format: raw`.

### `Morpheus reported virtual image N as failed`

Morpheus rejected the uploaded image. Inspect the virtual image in the Morpheus UI. The most likely cause is a format the appliance cannot convert; try the other `image_format`.

### An import failed and now every machine fails

A failed import is dropped from the in-memory cache so the next Machine Request retries it cleanly. If an *incomplete* virtual image record was left behind in Morpheus, the provider deletes it as part of handling the failure — but if that deletion also failed, the error says so and names the image ID. Delete it by hand, or the name lookup will keep finding an empty image and every machine will provision from it.

## Provisioning problems

### `Morpheus instance "..." failed to provision`

Morpheus finished and failed. The provider stops retrying, because polling cannot change the outcome, and leaves the instance in place so you can inspect it. Open the instance in Morpheus and read its history/logs.

Common causes: no capacity in the resource pool, a network the layout cannot attach, or a service plan incompatible with the layout.

### Nodes boot but never join Omni

The VM is running in Morpheus but never appears in Omni. This is almost always the join config not reaching Talos — see [Compatibility](compatibility.md#1-cloud-init-user-data-passthrough), which explains why this is the most likely failure mode of this port.

To confirm, compare what Morpheus rendered against what Omni supplied:

```bash
curl -sk -H "Authorization: Bearer $MORPHEUS_TOKEN" \
  "$MORPHEUS_ENDPOINT/api/instances/<id>" | jq -r '.instance.config.userData'
```

That must be **byte-identical** to the Omni join config. If Morpheus has wrapped it in `#cloud-config`, appended a `users:` block, or added an agent-install script, Talos will have failed to parse it.

If it has, verify that `createUser` and `noAgent` took effect on the instance, and check whether a **provisioning policy** or the **cloud's agent install mode** is re-enabling guest customization at the appliance level — those override what the request asks for. Setting the cloud's agent install mode away from `cloudInit` is the usual fix.

Open the VM console in Morpheus to see what Talos itself reports. A node that read a corrupt config says so on the console.

### The console is blank

The provider requests `console=ttyS0,38400n8 console=tty0` as kernel arguments, with `tty0` last so it owns `/dev/console`. If the layout gives the guest no serial port and `tty0` were absent, every message after early boot would go to a device that does not exist. If the console is blank anyway, check the VM actually powered on rather than halting immediately — see the firmware note in [Compatibility](compatibility.md#5-boot-firmware).

## Deprovisioning problems

### A machine will not deprovision

The provider powers the instance off, then deletes it with `removeVolumes=true` and `force=true`, retrying until the instance is gone. An instance still provisioning is left alone until it settles, because Morpheus will not remove it cleanly mid-build.

If it is stuck, check the instance in Morpheus for a pending action or an approval policy holding the delete.

### A renamed instance was left behind

The provider records the Morpheus instance ID at creation and prefers it over the name when deprovisioning, so renaming an instance in Morpheus does not strand it. If the recorded ID is gone from Morpheus, the machine is treated as already deprovisioned.

## Build problems

### `undefined: http2.TrailerPrefix`

Not a problem with this project. Go 1.27 changed how `golang.org/x/net/http2` is built, and gRPC 1.81 has not caught up: `x/net` excludes the file defining that constant under `//go:build !(go1.27 && !http2legacy)`.

Build with the Go version the module targets, which is what CI and the Dockerfile use:

```bash
GOTOOLCHAIN=go1.26.2 go build ./...
```

`GOTOOLCHAIN=auto` will not do this for you — it only ever selects a *newer* toolchain than the one installed, never an older one.
