# The Arca OS kernel

Arca OS runs Android containers, which need Android binder in the host kernel. IncusOS installs
[Zabbly's](https://github.com/zabbly/linux) mainline kernel, and Zabbly's configuration does not enable binder. So
Arca OS rebuilds Zabbly's own tree with binder built in, and changes nothing else.

## Why Zabbly's tree, and not mainline

Zabbly's `x86_64` branch is a stable mainline release plus Zabbly's patches. One of them lets the kernel check module
signatures against the platform (UEFI db) keyring. IncusOS signs its DKMS modules (ZFS, DRBD) with its Secure Boot db
key during the image build, so a kernel without that patch would refuse to load them. Building Zabbly's tree with
Zabbly's committed `.config` keeps that, and every other choice Zabbly made.

## What changes

[`arca.config`](arca.config) is the whole change, four lines merged into Zabbly's `.config`:

```text
CONFIG_ANDROID_BINDER_IPC=y
CONFIG_ANDROID_BINDERFS=y
CONFIG_ANDROID_BINDER_DEVICES=""
CONFIG_LOCALVERSION="-zabbly-arca"
```

- Binder and binderfs are built in (`=y`), so they cannot be missing as modules.
- No binder device is created at boot. Each Android container mounts its own binderfs instance and allocates its
  devices through `binder-control`.
- The `-zabbly-arca` suffix keeps the kernel release apart from Zabbly's (`<version>-zabbly-arca+` against
  `<version>-zabbly+`), and still matches the image's `RemoveFiles=/boot/*zabbly*`.

## How it is built

```sh
kernel/build-kernel.sh --out <root> [--ref <commit>]   # builds <root>/<krel>-<stamp>/ and points <root>/latest at it
kernel/build-kernel.sh --verify-config <file>          # exit 0 if all four lines are in <file>, else 3, naming each missing line
kernel/build-kernel.sh --detect-upstream <file>        # prints upstream-binder: yes|no
kernel/test/check-fragment.sh                          # tests the fragment and both modes on small fixtures
```

`build-kernel.sh --out` runs on Debian trixie (the builder VM, see `scripts/arca/README.md`) and:

1. reads the head of Zabbly's `x86_64` branch, and shallow-clones the tree at `--ref` (default: that head);
2. records `make kernelversion` (`kver`) and a UTC build stamp (`stamp`, `YYYYMMDDHHMM`);
3. merges `arca.config` into the committed `.config` with `scripts/kconfig/merge_config.sh -m`, runs
   `make olddefconfig`, and fails with exit 3 if any of the four lines did not survive;
4. runs `make bindeb-pkg` with `KDEB_PKGVERSION=<kver>-amd64-<stamp>-arca` and
   `KDEB_SOURCENAME=linux-zabbly-arca-<KDEB_PKGVERSION>`, which gives `linux-image-<krel>` and `linux-headers-<krel>`;
5. builds the meta packages `linux-image-zabbly` and `linux-zabbly` with `equivs-build` from [`meta/`](meta), at
   version `<stamp>~amd64~arca`. They depend on exactly this build's image (and headers), and carry the names the
   image build installs, so IncusOS's package lists need no change.

The output directory holds:

| Path | Content |
|---|---|
| `debs/` | the image, headers and two meta packages: what the image build installs |
| `extra/` | the `-dbg` and `linux-libc-dev` packages, the dpkg build records, and Zabbly's unmodified `.config` |
| `config` | the final `.config`, the same file as the image's `/boot/config-<krel>` |
| `arca.config` | the fragment used |
| `source.tar.xz` | `git archive` of the exact Zabbly commit built |
| `BUILDINFO` | `zabbly-commit`, `kver`, `krel`, `stamp`, `version`, the sha256 of `config`, `arca.config` and `source.tar.xz`, the branch head read at build time, the meta version and `upstream-binder` |

## Keeping Zabbly's kernel out of the image

The image build still uses Zabbly's kernel repository, because its `zfs` component provides the ZFS DKMS sources.
[`mkosi.sandbox/etc/apt/preferences.d/arca-kernel.pref`](../mkosi.sandbox/etc/apt/preferences.d/arca-kernel.pref)
gives Zabbly's `linux-image-zabbly`, `linux-zabbly` and versioned `linux-image-*-zabbly+` and
`linux-headers-*-zabbly+` packages a priority of -1, so apt never installs them, and the meta packages come from the
Arca OS build instead. The image manifest is the check: it must list `linux-image-*-zabbly-arca*` and no
`linux-image-*-zabbly+`.

## The GPL

The kernel is GPL-2.0. Every published kernel ships with its corresponding source next to it: `source.tar.xz` (the
exact tree, which also survives a later force-push of Zabbly's branch), the final `config`, and `arca.config`. Rebuild
it with `build-kernel.sh --out <dir> --ref <zabbly-commit from BUILDINFO>`, or from the tarball with the same `config`.

## When Zabbly ships binder

`--detect-upstream` reads a Zabbly `.config` and reports `upstream-binder: yes` once both `CONFIG_ANDROID_BINDER_IPC=y`
and `CONFIG_ANDROID_BINDERFS=y` are set. Every build prints it for the tree it builds, and a daily check of the branch head
can use it to flag the change. Until then nothing changes, and after it the build still works, because merging the
fragment no longer changes binder.

Retiring this directory means deleting `kernel/`, the apt pin, and the Arca OS kernel apt source, so the image goes
back to Zabbly's packages.
