# Providers

IncusOS receives [updates](update.md) from the currently configured provider. Two providers are supported:

* `images`: The default IncusOS provider, which fetches updates from the [Linux Containers {abbr}`CDN (Content Delivery Network)`](https://images.linuxcontainers.org/os/).

* `operations-center`: When IncusOS is deployed in a managed environment controlled by [Operations Center](../applications/operations-center.md), it is registered with the `operations-center` provider. This allows an administrator to centrally control all IncusOS systems, even in restricted or air-gaped environments that may not have Internet access.

## Configuration options

Configuration fields are defined in the [`SystemProviderConfig` struct](https://github.com/lxc/incus-os/blob/main/incus-osd/api/system_provider.go).

The following configuration options can be set:

* `name`: The name of the provider. One of `images`, `operations-center`, or `debug`. `debug` is intended for use by developers working on IncusOS.

* `config`: A map of provider-specific configuration key-value pairs.

For the `images` provider, the `update_ca` key sets a PEM-encoded certificate authority used to verify signed update metadata. It must be the certificate authority that directly issued the signer's certificate (basic constraint `CA:TRUE`; a pinned leaf certificate is not accepted), and a signer issued under a further intermediate is not accepted. The `update_ca` certificate must itself chain to the root certificate built into the OS, and where it has a key usage extension, that extension must include Certificate Sign.
