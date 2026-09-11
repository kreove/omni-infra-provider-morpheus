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

The VM is running in Morpheus, the console shows Talos healthy in `Maintenance` stage, and the node never appears in Omni. The console banner shows `SIDEROLINK: n/a`.

This means Talos never received a machine config. The usual cause is that the machine is not reaching the provider's NoCloud server — the mechanism that exists precisely because Morpheus does not deliver user data to the guest (see [Compatibility](compatibility.md#1-cloud-init-user-data-passthrough-confirmed-broken-worked-around)).

Note that the logs are unhelpful here by design: Talos reports `found config disk (cidata)` and `fetching machine config from: cidata/user-data` and then silently discards what it read, because Morpheus's file starts with `#cloud-config`. **The absence of an error is expected and is not evidence that config delivery worked.**

Check, in order:

1. **Is the NoCloud server configured at all?** The provider logs a warning at startup if `NOCLOUD_SERVER_URL` is unset. Without it, no machine can ever get a config.

2. **Did the machine fetch its config?** The provider logs `served Talos machine config` with the hostname, once per machine. If that line never appears, the machine could not reach the server. The provisioning step stays at `waiting for the machine to fetch its Talos config` in Omni for ten minutes before giving up with a warning naming the URL.

3. **Can machines reach the URL?** `NOCLOUD_SERVER_URL` must be reachable *from the VM network*, which is usually not the same network the provider runs on. From a VM on that network:

   ```bash
   curl -v http://provider-host:9080/nocloud/test/user-data
   ```

   A `503` proves reachability (the token is unknown, which is the expected answer). A timeout or refusal is the problem. Note Talos only retries for **three minutes** before giving up until the next boot.

4. **Did the SMBIOS serial reach the guest?** On the hypervisor:

   ```bash
   virsh dumpxml <domain> | grep -A8 "qemu:commandline"
   ```

   It should contain `-smbios type=1,serial=ds=nocloud-net;s=http://...`. If it does not, Morpheus dropped `config.qemuArgs` — check for a provisioning policy restricting it. Inside a guest that has a shell, `dmidecode -s system-serial-number` shows what Talos actually sees; a bare UUID there means the override did not take.

If a machine missed its three-minute window — for example because the provider was restarted while it was booting — rebooting the VM makes it ask again.

### The console is blank

The provider requests `console=ttyS0,38400n8 console=tty0` as kernel arguments, with `tty0` last so it owns `/dev/console`. If the layout gives the guest no serial port and `tty0` were absent, every message after early boot would go to a device that does not exist. If the console is blank anyway, check the VM actually powered on rather than halting immediately — see the firmware note in [Compatibility](compatibility.md#6-boot-firmware-settled).

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
GOTOOLCHAIN=go1.26.7 go build ./...
```

`GOTOOLCHAIN=auto` will not do this for you — it only ever selects a *newer* toolchain than the one installed, never an older one.
