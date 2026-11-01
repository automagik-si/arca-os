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

Write the image to a stick (`/dev/sdX` is the stick, and is wiped), and boot the laptop from it:

```sh
sudo dd if=<ImageId>_<version>-installer.img of=/dev/sdX bs=4M conv=fsync oflag=direct status=progress
```

When it asks for the install media to be removed, remove the stick. The laptop reboots into the installed system,
which downloads `incus` and `arca` from the server and installs them. Stop the server once both are listed. If the
server could not be reached during that boot, every later boot tries again until a primary application is installed.

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
