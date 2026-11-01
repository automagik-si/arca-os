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
outputs persist under `/srv/artifacts/` in the VM. Defaults: `--project arcaos-dev --vm arcaos-builder`.

## Building and booting

```sh
scripts/arca/in-builder.sh "$PWD" -- scripts/arca/build.sh          # or build.sh --iso
scripts/arca/vm-test.sh --project arcaos-dev --vm arcaos-stock --from-builder arcaos-builder --stop-builder \
  --seed-apps incus,debug --provider debug --checks stock
```

`build.sh` runs upstream's `make build` (test certificates are generated when there is no `mkosi.crt`) and copies the
image, the `.efi`, the `usr` images, every sysext, the manifests and the tree's `test/metadata.tar.xz` to
`/srv/artifacts/os/<version>/`, with `/srv/artifacts/os/latest` pointing at the newest. `vm-test.sh` takes the image,
its metadata and the expected ImageId from there, not from the checkout it runs in, and never accepts the builder's
name as `--vm`.

`vm-test.sh` follows upstream's end-to-end job: it injects a seed (`applications.json`, `provider.json`) into a copy of
the image, imports it, boots a VM (4 vCPU, 8 GiB, 50 GiB root, vTPM, Secure Boot keys enrolled by the image itself),
pushes the seeded applications for the `debug` provider, and waits for incus-osd to list them. Then it prints one
`PASS <check>` or `FAIL <check>` line per check and exits 1 on any FAIL. The `stock` checks are `boot`, `app-listed`,
`secureboot`, `os-id` and `mode`. It refuses to start the VM when the host has less than 12 GiB of `MemAvailable`
(after stopping the builder, with `--stop-builder`). Large files are staged under `~/.cache/arca-os/vm-test`
(`ARCA_VM_TEST_DIR` overrides it), not under `/tmp`.

## Component map

| Component | Files |
|---|---|
| Builder, toolchain, stock build and boot | `scripts/arca/{provision-builder,dev-vm,in-builder,build,vm-test}.sh` |
| Binder kernel from Zabbly's tree | `kernel/`, `mkosi.sandbox/etc/apt/preferences.d/arca-kernel.pref` |
| `arca` as an IncusOS application | `incus-osd/internal/applications/app_arca*.go`, `mkosi.images/arca/`, `scripts/arca/build-arca.sh` |
| Name, update URL, notice, patch-stack guard | `NOTICE`, `scripts/arca/PATCHES.md`, `scripts/arca/check-patch-stack.sh` |
| Signing keys | `scripts/arca/keys/` |
| Update server | `scripts/arca/r2/` |
| Kernel apt repository | `kernel/publish-apt.sh`, `.github/workflows/arca-kernel.yml` |
| CI runner | `scripts/arca/runner/` |
| Upstream sync, build and publish | `scripts/arca/ci/`, `.github/workflows/arca-{sync,build}.yml` |
| Installation | `scripts/arca/install/` |

Rows other than the first describe files that later changes add.
