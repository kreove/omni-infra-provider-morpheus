# Security policy

## Reporting a vulnerability

Do not open a public issue for a suspected vulnerability involving credentials, authorization bypass, secret disclosure, unsafe image fetching, or destructive infrastructure behavior.

Use the repository's private security-advisory feature or contact the maintainer through the private security address published with the repository. Include:

- Affected version or commit
- Reproduction steps
- Security impact
- Suggested mitigation, if known

Do not include live Omni service-account keys, Morpheus API tokens, Talos join tokens, or other credentials.

## Supported versions

Until the project reaches a stable release, security fixes are provided for the latest published release only.

## Secret handling

Treat these values as secrets:

- `OMNI_SERVICE_ACCOUNT_KEY`
- `XOA_TOKEN`
- `XOA_PASSWORD`
- Machine Join Config and embedded join tokens

Store secrets in a container secret store, protected environment file, or Kubernetes Secret. Do not commit them to source control.

## Image Factory URL security

The provider does not accept an Image Factory URL from any source. It asks Omni for the installation medium it needs, and Omni returns the URL. Machine Class data cannot redirect the provider to an arbitrary host, and neither can provider configuration.

**The returned URL may contain credentials**, as userinfo or as a download token in the query string, depending on how the factory Omni talks to is configured. The provider treats it as a secret:

- It is never written to a log line.
- It is never stored in the Morpheus virtual image description, which records the Talos version, architecture and schematic instead.
- It is never used to derive the cached image name, which comes from the medium's storage key.

Keep this in mind if you add logging around the import path.

Operators should:

- Trust only controlled certificate authorities.
- Restrict outbound access from the provider container where appropriate.
- Review private Image Factory access controls in Omni.

The provider downloads images itself rather than having Morpheus fetch them, so the provider host needs outbound HTTPS access to the Image Factory — factor this into your network segmentation.

## NoCloud config server

The provider listens on an HTTP port and serves each machine its Talos configuration. This exists because Morpheus does not deliver cloud-init user data to the guest; see [Compatibility](docs/compatibility.md#1-cloud-init-user-data-passthrough-confirmed-broken-worked-around).

**What is served is a credential.** The Talos machine configuration contains the join token a machine uses to register itself with Omni. Treat the endpoint as you would any credential distribution point.

How it is protected, and how it is not:

- Each machine's datasource is addressed by a **random 256-bit token**, minted per machine and never derived from the machine request ID. Knowing the token is the only authorization; there is no other authentication.
- The token is **not a secret from the machine's environment**. It appears in the VM's SMBIOS serial, in the Morpheus instance's QEMU arguments, and therefore to anyone who can read that instance in Morpheus or run code in the guest.
- **Plain HTTP means the config crosses the network in the clear.** Anyone able to observe traffic between a provisioned VM and the provider can read the join token. Put the endpoint on a network you trust for that, or terminate HTTPS in front of it and set `NOCLOUD_SERVER_URL` to the HTTPS address — the guest must trust the certificate.
- The server exposes **only** `user-data`, `meta-data` and `network-config` under `/nocloud/<token>/`. It has no other endpoints and no write paths.
- An unknown token is answered with `503`, not `404`, because Talos treats a 404 as a definitive "no config" and stops asking. This means the endpoint does not distinguish an unregistered machine from a wrong token, which is also the behaviour that avoids confirming whether a guessed token exists.
- Entries live in memory only and are dropped when the machine is deprovisioned. A provider restart forgets them until the next reconcile republishes.

Restrict the port to the network the provisioned VMs are on. It does not need to be reachable from operators, from Omni, or from the internet.

The join config is **not** also handed to Morpheus when this server is in use. Sending it would write the join token onto a config drive readable by anyone with access to the instance, for no benefit: the guest reads its config over HTTP and ignores the drive.

## Least privilege

Use a dedicated Morpheus API user scoped to the resources required by this provider: listing clouds, groups, networks, layouts and plans; creating and deleting instances; and creating, uploading and deleting virtual images. Avoid administrator credentials for long-running deployments.

Prefer an API token over a username and password. A token can be revoked independently of the account, and it avoids requiring the account to hold a local password at all.
