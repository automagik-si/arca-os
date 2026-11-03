# Arca OS build scripts

Arca OS is [IncusOS](https://github.com/lxc/incus-os) with two additions:

- a kernel with Android binder built in (`CONFIG_ANDROID_BINDER_IPC=y`, `CONFIG_ANDROID_BINDERFS=y`), so Android
  containers can run on the host;
- `arca`, a native IncusOS application that talks to the local Incus socket.

Everything else is upstream. The fork is a thin patch stack on branch `arca`, rebased onto each upstream tag. Arca-owned
files live under `scripts/arca/`, `kernel/` and `mkosi.images/arca/`, so upstream files change as little as possible.

## The builder

The build needs root, loop devices and about 130 GB of scratch space, so it runs in a dedicated Debian 13 VM, the
builder, in its own Incus project. Nothing in these scripts needs root on the Incus host.

```sh
# On the Incus host: project arcaos-dev, VM arcaos-builder (8 vCPU, 16 GiB, 200 GiB root).
scripts/arca/dev-vm.sh create --pool <storage-pool> --network <bridge>
scripts/arca/dev-vm.sh status
scripts/arca/dev-vm.sh stop
scripts/arca/dev-vm.sh destroy        # deletes the project's instances, images and profiles, then the project
```

`dev-vm.sh` only acts on projects whose name starts with `arcaos-` (`--project arcaos-<name>`); any other project is
refused with exit 2. An existing project must also have its own images and profiles (`features.images=true` and
`features.profiles=true`); otherwise its image and profile calls would reach the default project's, so `create`,
`destroy` and `vm-test.sh` refuse it with exit 2. `create` adds a `root` disk on the given pool and an `eth0` NIC on the given bridge to the
project's `default` profile, launches `images:debian/13` as a VM, and runs `provision-builder.sh` in it. It does not
start the builder when the host has less than 20 GiB of `MemAvailable`.

`provision-builder.sh` (run as root on Debian 13, idempotent) installs the toolchain:

| Tool | Pin |
|---|---|
| mkosi | `v27.1`, through pipx, at `/usr/local/bin/mkosi` |
| Go | newest `1.27.x` from go.dev, sha256-checked, at `/usr/local/go` (`GO_VERSION=go1.27.N` pins it) |
| Rust | rustup, toolchain `1.97.1` with `x86_64-unknown-linux-musl`, shared under `/usr/local/rustup` |
| Debian packages | upstream's image-build list, Zabbly's kernel-build list, and `efitools`, `equivs`, `reprepro`, `rclone`, `musl-tools`, `shellcheck` and friends |

It ends with one version line per tool.

## Running things in the builder

```sh
scripts/arca/in-builder.sh <worktree> -- <command...>
```

`in-builder.sh` refuses a dirty worktree (exit 2). It ships the worktree's `HEAD` with `git bundle`, checks it out
clean at `/srv/work/<worktree name>` in the VM, and runs the command there as root, returning its exit code. Build
outputs persist under `/srv/artifacts/` in the VM. Defaults: `--project arcaos-dev --vm arcaos-builder`. The worktree's
`git config user.name` and `user.email`, when both are set, become the VM checkout's identity (sent on stdin, never
printed), so `kernel/build-kernel.sh` can take its Debian `Maintainer` from them; `--maintainer '<name> <email>'`
overrides that, and with neither the kernel build is refused.

## The persistent test CA

A build embeds a test CA: the update CA (which signs the update index a node checks), the Secure Boot keys and the
signing certificate for the UKI and the `usr`-verity image. Upstream's `make` generates a fresh CA whenever `./certs/`
is absent, and `in-builder.sh`'s `git clean -ffdx` makes it absent on every run. A fresh CA each build would orphan
every node already installed: it could no longer verify a later build's signed index or `usr`-verity signature, and
could never be sent a verifiable update again.

So the CA is generated once and kept outside any checkout, and copied into each checkout before the build:

```sh
# Once, in the builder, from the top of a checkout. Stores the CA at
# /srv/arca-test-ca (ARCA_TEST_CA_DIR overrides it). Then copy it off the
# builder and keep it safe; losing every copy orphans every installed node.
scripts/arca/provision-test-ca.sh
```

`build.sh` runs `inject-test-ca.sh` before `make`: it copies the stored CA into the checkout's `certs/` and
`incus-osd/certs/files/`, recreates the `mkosi.crt`/`mkosi.key` symlinks, and verifies the update CA and Secure Boot
signing certificate against the fingerprints pinned in the CA set's `FINGERPRINTS`. A build **refuses** when the CA set
is missing or a fingerprint does not match, rather than falling back to a throwaway CA. `inject-test-ca.sh --check`
verifies the CA set without touching a checkout.

## Building and booting

```sh
scripts/arca/in-builder.sh "$PWD" -- scripts/arca/build.sh          # or build.sh --iso
scripts/arca/vm-test.sh --project arcaos-dev --vm arcaos-stock --from-builder arcaos-builder --stop-builder \
  --seed-apps incus,debug --provider debug --checks stock

# With the binder kernel and the arca sysext:
scripts/arca/in-builder.sh "$PWD" -- scripts/arca/build.sh --local-kernel /srv/artifacts/kernel/latest
scripts/arca/vm-test.sh --project arcaos-dev --vm arcaos-tracer --from-builder arcaos-builder --stop-builder \
  --seed-apps incus,arca,debug --provider debug --checks tracer
```

`build.sh` always runs `build-arca.sh` first, which builds `arca` at the tag in `arca.version` and refuses a tag that
no longer points to the commit in `arca.commit`. With `--local-kernel <dir>`, the kernel build's debs go into
`mkosi.packages/` (mkosi's local repository) for that build only, and its `BUILDINFO` goes beside the image as
`kernel.BUILDINFO`.

`build.sh` runs upstream's `make build` (test certificates are generated when there is no `mkosi.crt`) and copies the
image, the `.efi`, the `usr` images, every sysext, the manifests and the tree's `test/metadata.tar.xz` to
`/srv/artifacts/os/<version>/`, with `/srv/artifacts/os/latest` pointing at the newest. `vm-test.sh` takes the image,
its metadata and the expected ImageId from there, not from the checkout it runs in, and never accepts the builder's
name as `--vm`.

`vm-test.sh` follows upstream's end-to-end job: it injects a seed (`applications.json`, `provider.json`) into a copy of
the image, imports it, boots a VM (4 vCPU, 8 GiB, 50 GiB root, vTPM, Secure Boot keys enrolled by the image itself),
pushes the seeded applications for the `debug` provider, and waits for incus-osd to list them. Then it prints one
`PASS <check>` or `FAIL <check>` line per check and exits 1 on any FAIL. The `stock` checks are `boot`, `app-listed`,
`secureboot`, `os-id` and `mode`. The `tracer` checks add `kernel`, `binder-fs`, `binder-major`, `binderfs-mount`,
`arca-active`, `arca-call`, `zfs` and `kernel-pkg`; `arca-call` needs `arca call watch_report` to exit 0 and name
the project `default`, and `zfs` needs the module to load with lockdown `integrity` or `confidentiality`. The kernel
release can end in `+`, which `apt-get` reads as an
install modifier and a regular expression as a repeat, so `vm-test.sh` compares it as a fixed string only. It refuses
to start the VM when the host has less than 12 GiB of `MemAvailable`
(after stopping the builder, with `--stop-builder`). Large files are staged under `~/.cache/arca-os/vm-test`
(`ARCA_VM_TEST_DIR` overrides it), not under `/tmp`.

## A test installer

`tracer-installer.sh` writes a test-signed build as one USB installer image, plus the update tree that the installed
system fetches `incus` and `arca` from. It runs in the builder right after `build.sh`, in the same checkout, before
anything cleans it, because it signs with that build's throwaway test update key. `--server-url` is required: it is
the URL the tree will be served at, and it goes into the image's seed.

```sh
# In the builder, after build.sh, in the same /srv/work/<name> checkout (not through in-builder.sh, which cleans it):
scripts/arca/tracer-installer.sh --server-url http://<address>:<port> --client-cert - --force-install < client.crt
# On the Incus host, an install rehearsal from those files (<address> is this host's address on the VM's network):
scripts/arca/vm-test.sh --project arcaos-dev --vm arcaos-installer --install-media <dir> --checks tracer
```

It writes, in one directory:

- `<ImageId>_<version>-installer.img`, the image with an install seed: applications `incus` and `arca`, provider
  `images` at that `server_url`, channel `stable` with updates checked `never` and no automatic reboot,
  `force_install` only with `--force-install`, and with `--client-cert` that certificate trusted by Incus. It installs
  onto the one disk it finds and refuses when it finds several.
- `updates/`, a static tree in the layout incus-osd's `images` provider reads, made by the build's own
  `image-publisher`: `index.json` and `index.sjson`, and `<version>/` with `update.json`, `update.sjson`,
  `x86_64/incus.raw.gz` and `x86_64/arca.raw.gz`, in channel `stable`. Both `.sjson` files are signed with the build's
  test update key, which the image trusts, so plain `http` is enough. The tree holds no OS files, so the OS is never
  updated from it. The script reads it back the way incus-osd will: both signatures, one release, and exactly the two
  applications, each unpacking to the built sysext.
- `seed/`, `RELEASE`, `OSNAME`, `base.manifest`, `kernel.BUILDINFO` and `SHA256SUMS`, which covers the image and every
  file under `updates/`.

On its first boot the installed system has no primary application, so incus-osd checks the server once even though
updates are checked `never`, and installs `incus` and `arca`. After that it does not check again on its own. The
target's firmware must be in Secure Boot Setup Mode, because the first boot enrolls the build's test keys.

With `--install-media <dir>`, `vm-test.sh` rehearses this. The seed's `server_url` must be `http://<address>:<port>`,
with `<address>` this host's address on the network of the project's default `eth0` and `<port>` (1024 or above) free
on every address; otherwise it refuses with exit 2. It boots an empty VM from a copy of the image as a USB disk, waits
for the install, stops the VM and detaches the disk, serves `updates/` on that address with a throwaway
`python3 -m http.server`, and boots the installed system without asking it for an update check. Besides the `tracer`
checks it checks `installed`, `media-removed`, `apps-from-server` (the server answered the VM's `GET` of `index.sjson`
and of each application's `.raw.gz` with 200) and `updates-off` (check frequency `never`, no automatic reboot, provider
`images` at the seed's URL). It always stops the server on exit. On any exit before the install disk is detached, it
also stops the VM and detaches the disk.

### Installing a laptop

Serve `updates/` at the `--server-url` the image was built with, from a machine on the laptop's network, until the
laptop has installed both applications. Any static file server works. With Python, from the installer directory:

```sh
sha256sum -c SHA256SUMS
python3 -m http.server --bind <address> --directory updates <port>
```

Write the image to a stick with `make-usb.sh`, on Linux or macOS, from the installer directory. It lists the USB
sticks (never the system disk), asks which one to use and for its name to be typed back, since the whole stick is
erased. It then checks the image against `SHA256SUMS`, writes it with a progress bar, reads the stick back and
prints the steps for booting the installer:

```sh
scripts/arca/make-usb.sh                 # the newest *-installer.img in the current directory
scripts/arca/make-usb.sh --list          # only list the sticks
scripts/arca/make-usb.sh --dry-run IMG   # every step except the write
```

Then boot the laptop from the stick.

When it asks for the install media to be removed, remove the stick. The laptop reboots into the installed system,
which downloads `incus` and `arca` from the server and installs them. Stop the server once both are listed. If the
server could not be reached during that boot, every later boot tries again until a primary application is installed.

## Releasing updates to nodes

Once a build exists at `/srv/artifacts/os/<version>/`, publish it into the update tree that nodes check. The tree uses
the layout and the signed `index.sjson`/`update.sjson` that incus-osd's `images` provider reads; both are signed with
the persistent update key, so any node installed from a build with the same CA verifies them over plain `http`.

```sh
# On the build host. Publishes the OS (UKI + usr images) and the incus and arca
# applications, and copies the tree to a local directory to serve.
scripts/arca/publish-update.sh --version <version> --serve-dir <dir> --channel stable

# Serve that directory to the node's network. Any static file server works; the
# LAN address and port are chosen here, at serve time, not baked into anything:
python3 -m http.server --bind <lan-address> --directory <dir> <port>
```

A node whose provider `server_url` is `http://<lan-address>:<port>` and whose update check frequency is a real interval
then finds and applies the newest build in its channel on its own. Republishing a later `--version` into the same tree
adds it; the node moves to it at its next check (and reboots if `auto_reboot` is set). The OS files make it an OS
update; leave them out (publish only applications) by passing `--apps` with no OS — the usual case is to publish both.

New sticks that should keep themselves current are written with `tracer-installer.sh --auto-updates`, which seeds the
update policy as check-frequency `1h` and `auto_reboot true` (the default is `never`/`false`, for a one-off test
stick). Valid check frequencies are `never` or any positive Go duration (`30m`, `1h`, `24h`).

`publish-build.sh` is the builder-side half that `publish-update.sh` runs through `in-builder.sh`; it signs and
verifies the tree inside the builder. Keep the served tree and the persistent CA backed up together.

## Migrating a node without reinstalling

A node installed from a build whose CA no longer exists cannot verify any signed update, so it can never be updated
through the normal path. `migrate-node.sh` moves such a node onto a persistent-CA build **without reinstalling**, using
the node's own Incus (which this host admins): a privileged container, given the node's disk and TPM, runs
`arca-node-migrate` in the node's context to write the new build's UKI and `usr` images the way
`systemd-sysupdate` does, and to re-seal the TPM for the new UKI. The node then boots the new build and updates itself
normally. `arca-node-migrate` lives in the `incus-osd` module and reuses its own update-apply code
(`secureboot.UpdatePCR4Binding`, `systemd-sysupdate`, the boot-selection helpers); it only supports a
Secure-Boot-disabled node and refuses otherwise.

```sh
# Build the one-shot helper (static) in the builder, once:
in-builder.sh <checkout> -- sh -euc 'cd incus-osd && CGO_ENABLED=0 go build -o /srv/artifacts/arca-node-migrate ./internal/arca/nodemigrate'
# then place it beside the script as scripts/arca/.arca-node-migrate.

scripts/arca/migrate-node.sh --remote <node-remote> --os-api https://<node>:8443/os/1.0 \
    --version <12-digit> --build-dir /srv/artifacts/os/<version> \
    --server-url http://<lan-address>:<port> --check-frequency 1h
```

**TPM re-seal.** The preferred path re-seals before reboot, so the next boot unlocks with no keyboard: the future PCR4
is computed from the new UKI and the LUKS volumes are rebound with the node's recovery key. On a Secure-Boot-disabled
node the binding is PCR 4+7+12; PCR7 is unchanged (Secure Boot stays off, the EFI variables and the old boot stub stay),
so only PCR4 (the new UKI) and PCR12 move. If re-sealing cannot run in the container, the fallback needs the keyboard
once: boot the new build with the recovery passphrase, then `POST /os/1.0/system/security/:tpm-rebind`
(`ForceUpdatePCRBindings`), which reseals from the now-running system and reboots to an automatic unlock.

Always rehearse in a VM first (project `arcaos-dev`): reproduce the node's conditions, migrate, and prove the new
version boots, unlocks without the recovery key, and keeps its data, then publish a second build and prove the node
picks it up on its own.

## Component map

| Component | Files |
|---|---|
| Builder, toolchain, stock build and boot | `scripts/arca/{provision-builder,dev-vm,in-builder,build,vm-test}.sh` |
| A test-signed USB installer | `scripts/arca/tracer-installer.sh` |
| Binder kernel from Zabbly's tree | `kernel/`, `mkosi.sandbox/etc/apt/preferences.d/arca-kernel.pref` |
| `arca` as an IncusOS application | `incus-osd/internal/applications/app_arca*.go`, `mkosi.images/arca/`, `scripts/arca/{build-arca.sh,arca.version,arca.commit}` |
| Name, update URL, notice, patch-stack guard | `NOTICE`, `scripts/arca/PATCHES.md`, `scripts/arca/check-patch-stack.sh` |
| Signing keys | `scripts/arca/keys/` |
| Update server | `scripts/arca/r2/` |
| Kernel apt repository | `kernel/publish-apt.sh`, `.github/workflows/arca-kernel.yml` |
| CI runner | `scripts/arca/runner/` |
| Upstream sync, build and publish | `scripts/arca/ci/`, `.github/workflows/arca-{sync,build}.yml` |
| Installation | `scripts/arca/install/` |

Rows other than the first describe files that later changes add.
