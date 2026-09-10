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

## Least privilege

Use a dedicated Morpheus API user scoped to the resources required by this provider: listing clouds, groups, networks, layouts and plans; creating and deleting instances; and creating, uploading and deleting virtual images. Avoid administrator credentials for long-running deployments.

Prefer an API token over a username and password. A token can be revoked independently of the account, and it avoids requiring the account to hold a local password at all.
