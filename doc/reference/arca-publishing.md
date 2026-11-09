---
orphan: true
---

# Arca OS automatic publishing

Arca OS publishes its own updates. When a change is merged into the publish branch, the build host builds it, signs
it, publishes it to a test channel, proves it on a throwaway test machine, and only then hands the same signed release
to every node on the stable channel. Nobody has to press a button, and nothing reaches a real machine before it worked
on a test machine.

This page describes that flow for whoever runs the build host. The scripts live in `scripts/arca/`.

## The update tree and its channels

Nodes read a static update tree: `index.json` lists every release, each in one or more channels, and each node takes
the newest release in the channel it is configured for. Every JSON file a node reads comes with a signed copy
(`.sjson`), signed with the persistent update key that every Arca OS build trusts.

Arca OS uses two channels:

- `stable` is what real machines follow. A release gets here only after a test machine updated to it and passed.
- `testing` is where every new build lands first. Only test machines follow it.

The tree is kept in the builder (`/srv/artifacts/updates`), where the signing key is, and mirrored to the directory
that the build host serves to the network (`~/.cache/arca-os/updates` by default).

## One run

`scripts/arca/autopublish.sh` does one run. A run picks the first of these that applies:

1. **Record.** A verdict for the newest automatically published release is saved on the build host but the tree does
   not show it yet, because an earlier run stopped after its test. The run applies it, without testing again.
1. **Resume.** The newest automatically published release is still in `testing` without a verdict, because an earlier
   test could not run. The run tests it again, without building.
1. **Publish.** The publish branch (`main` by default) moved since the newest automatically published release. The run
   builds the new commit in the builder, publishes it into `testing` with `publish-update.sh`, writes its release
   notes, and tests it.
1. **Nothing.** The branch has not moved, or its newest commit failed to build or publish recently. The run ends at
   once.

The builder is started for the build and stopped at the end of every run. Builds leave
`/srv/artifacts/os/latest`, which `vm-test.sh` boots by default, where it was (`build.sh --no-latest`).

A build is refused while the builder has less than 30 GiB free or its storage pool less than 50 GiB
(`MIN_BUILDER_FREE_GIB`, `MIN_POOL_FREE_GIB`). A build or publish that fails is removed again: its build output is
deleted and, if it reached the tree, it is marked failed there. The same commit is tried again after 30 minutes, then
after an hour, and after three failures (`BUILD_TRIES`) only a new commit is built.

## The canary

The test machine is the canary, run by `scripts/arca/canary.sh`:

1. A throwaway virtual machine is installed from the current `stable` release, as a node on the `stable` channel would be. It must
   come up on that release with its applications (`incus` and `arca`) installed. If it does not, the result is
   *inconclusive*: the new release was never tried.
1. The virtual machine is switched to the `testing` channel with automatic reboots, as an owner would switch a node, and asked to
   check for updates. It must download the new release, restart into it on its own, and pass the checks: it runs the new
   version, its applications are at the new version, the Incus API answers, and the `arca` application is up and
   answers a call.
1. When the node's own health bake is available, `--bake-jq` (or `CANARY_BAKE_JQ` in the configuration) names where the
   update state reports it, and the canary also waits for the node to mark the release good.

The canary virtual machine gets two CPUs and the same disk read and write limits as the builder, and it is deleted when the test
ends. The update tree is served to it from the build host for the length of the test.

## Promotion and failure

- **Passed.** The release is promoted to `stable`: the very files that were signed and tested gain the `stable`
  channel, and nothing is rebuilt. Its `canary.json` records the pass.
- **Failed.** The release's `canary.json` records `failed` and the reason in plain words, and the release leaves
  `testing`, so no other test machine takes it. It is never promoted. The commit is not built again unless the branch
  moves to a new commit.
- **Inconclusive.** The release stays in `testing` and the next run tests it again. Every test counts, and after three
  (`CANARY_TRIES`) it is marked failed.

A test that stops without a verdict counts as inconclusive if it stopped before the switch to `testing`, and as failed
after it, because the new release was running. Each verdict is saved on the build host
(`~/.local/state/arca-os/autopublish/verdicts/`) before the tree is changed, so a run that stops in between is finished
by the next one, and a release that failed once is never promoted.

Each run writes a summary to `~/.local/state/arca-os/autopublish/last-run.json` and appends it to `history.jsonl` in the
same directory, next to the run's full log.

## Release notes

Every release carries `notes.json` (and the signed `notes.sjson`) beside its `update.json`, so a console can show what
is new. It lists the subjects of the commits merged into the publish branch since the previous automatically
published release, the commit that was built, and what the release does not include. For now that is the NVIDIA
drivers: the update publisher does not publish the `nvidia` and `nvidia-lts` applications yet.

## Retention

After a verdict, the tree keeps the newest three `stable` releases, the newest `testing` release, the newest
automatically published release (so its verdict stays visible), and every release that a known node reports as
running, about to boot, or kept as its fallback. Everything else is removed from the tree, from the served copy, and
from the builder's build outputs that automatic publishing made. Nodes are listed with `--nodes` (or `NODES`) as the
names of their Incus remotes.

A node whose update channel is empty takes the newest release in any channel, `testing` included, so it would get a
release before its test. Set every real node's channel to `stable`; a run warns about each listed node that has none.

## Locks

Only one run goes at a time. A run never waits for the builder: if an image build, a test virtual machine or a cargo
build holds the builder, it logs that it skipped and ends. It checks two builder locks: `~/.cache/arcaos-builder.lock`,
which other work on the build host takes around each builder use, and the default lock of `in-builder.sh` and
`vm-test.sh` under `$XDG_RUNTIME_DIR`. A cargo build is seen through `~/.cache/arca-cargo.lock`.

## Configuration

`~/.config/arca-os/autopublish.conf` holds `KEY=value` lines, read as data, not run. The keys are `BRANCH`, `REMOTE`,
`CHECKOUT`, `TOOLS_CHECKOUT`, `SERVE_DIR`, `BUILDER_TREE`, `PROJECT`, `BUILDER`, `CANARY_VM`, `NODES`, `APPS`,
`KEEP_STABLE`, `BUILD_ARGS`, `BUILD_TRIES`, `MIN_BUILDER_FREE_GIB`, `MIN_POOL_FREE_GIB`, `CANARY_TIMEOUT`,
`CANARY_TRIES`, `CANARY_BAKE_JQ` and `STATE_DIR`. Flags on the command
line win over the file.

`autopublish.sh --dry-run` prints what a run would do, including the release notes and what retention would remove,
and changes nothing.

## Running it every 30 minutes

`scripts/arca/units/` holds a user service and timer for the build host. Edit the service's `ExecStart` to the
checkout that holds the scripts, then install them:

```sh
install -D -m 0644 scripts/arca/units/arca-autopublish.service ~/.config/systemd/user/arca-autopublish.service
install -D -m 0644 scripts/arca/units/arca-autopublish.timer ~/.config/systemd/user/arca-autopublish.timer
systemctl --user daemon-reload
systemctl --user enable --now arca-autopublish.timer
```

User timers run only while the user's service manager does; `loginctl enable-linger` keeps it running without a login.
