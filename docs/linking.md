# Linking a second device

Linking is experimental. It lets agent sessions on a second machine, the **linked device**, join the hall that another machine keeps, the **home**. The home keeps the one ledger. The linked device sends its sessions' commands to the home over SSH and keeps nothing of the hall but what it needs to answer its hooks.

The feature is behind `CROSSHALL_EXPERIMENTAL_LINK=1`, set on the linked device only. Its commands are not in `crosshall help` yet.

## Before you link

- Both machines run a build with the same link version. `crosshall link` prints both releases and the version they share, and refuses when they share none.
- From the device, `ssh <home>` works without a prompt: key or agent authentication, no password. crosshall runs SSH in batch mode with agent forwarding and port forwarding off.
- The home account logs in to a POSIX-compatible shell whose startup prints nothing on a non-interactive session. crosshall finds the home's binary with `command -v crosshall`, falling back to `~/.local/bin/crosshall`.
- The home's dispatcher is running, or can start: the link starts it on demand like any other command.
- Install the device's adapter hooks with `crosshall setup` before you link. While the device is linked, `setup` and `uninstall` refuse, so unlink first to change them.

## Link

In your own shell on the device, not in a member session:

```sh
CROSSHALL_EXPERIMENTAL_LINK=1 crosshall link <ssh-target> [--name <device>] [--yes]
```

`<ssh-target>` is a host or an alias from your SSH config, optionally with `user@`. The command:

1. finds crosshall on the home and checks that the two builds share a link version;
2. prints what it will do, the two releases, and the estimated clock offset of the home;
3. asks `Link and enroll this device? [y/N]`, unless you pass `--yes` (a run with no terminal needs `--yes`);
4. enrolls the device on the home with a new secret, then starts the device's dispatcher, which connects and stays connected.

The device name defaults to the first label of the device's host name, lowercased. A name is 1 to 32 lowercase letters, digits or hyphens; use `--name` when the host name does not fit. Give each machine its own name. Linking under a name the home already knows replaces that name's secret and closes its links: if another machine held the name, it can no longer connect, and reconnecting does not help. That machine has to run `crosshall unlink` and link again under an unused name.

Run the same `link` again to restart the device's dispatcher. To link to a different home, unlink first.

## Status

```sh
CROSSHALL_EXPERIMENTAL_LINK=1 crosshall link status
```

shows the home, the state (`unlinked`, `stopped`, `connecting`, `connected` or `disconnected`), the link version and home release, the clock offset, and the last error. It reads the device's own record and does not contact the home. `--json` gives the same fields. A disconnected device reconnects by itself, waiting from one second up to a minute between tries.

## What runs on a linked device

A member session on the device uses the ordinary member commands, and the device sends each one to the home: `join`, `who`, `inbox`, `read`, `ask`, `reply`, `note`, `log`, `leave`, `focus` and `wait`. A join names its channel. `wait` polls the home about once a second while it runs, and the home shows the member as woken by its wait.

These stay on the home, and the device refuses them with a hint:

- Owner commands. The owner seat is on the home: `ssh -- <home> crosshall owner help`.
- `status`, `waiting`, `whoami` and `publish`, which the device does not forward yet.
- `join --takeover`, binding a join to an agent process, a repository's default channel, exit tracking, and owner takeover, move or restore of a device's member.

Adapter hooks run on the device. They answer from what the home last sent the device, so they do not wait for the network, and they go quiet while that copy is out of date. The hall's adapter settings come from the home. Repository auto-join, turn-end checks, and reach and join hints are off for linked sessions.

An idle session on the device can be woken by a [notice](adapters.md#publish-notices) when both builds support linked wakes: the device checks the session's route itself and sends the notice there. With a build that does not, the session sees new work at its next prompt or crosshall command. A message to a member on a disconnected device is stored and waits for the session. When the message would otherwise have woken that session, the sender's line says `device offline`.

## When the link is down

Nothing waits on the device to be sent later. A command run while the device is disconnected fails with `the link to the home is down; nothing was sent`. Run it again once `crosshall link status` says `connected`; right after a reconnect the device may still be checking its earlier commands, so a command that again says nothing was sent can be run again a moment later.

Every command that changes the hall is recorded on the device before it is sent, under its `--key` or a generated one. If the link ends before the home answers, the command says so and names its key. Then:

```sh
crosshall retry <key>
```

shows what the hall did with it. If the hall has no receipt for it, `retry` sends it once more as a new operation, which may repeat its effect. The device also looks up its open records, without running them again, each time it reconnects. The hall keeps receipts for 7 days.

## Unlink

```sh
CROSSHALL_EXPERIMENTAL_LINK=1 crosshall unlink
```

removes the device's link record and restarts its dispatcher alone. Sessions on the device then lose access to the home's hall. The home does not end their memberships: they stay listed in `crosshall who` and keep their aliases until the owner removes them on the home (`crosshall owner depart`). The home also keeps the device's enrollment and history; there is no home command to remove a device yet.

## Where the device keeps its state

In the device's state directory: `link.json` (the link), `link-status.json` (what `link status` shows), and while a link is being made, `link-enrollment.json`. The device's own store is `spoke/<hall>-<device>/store.sqlite3` there, with its sessions' spool directories under `/tmp/crosshall-<uid>-<hash>` as usual. The device runs its own dispatcher, which holds two SSH connections to the home: one for commands and one for the home's events. While linked, the device has no hall of its own.

## Limits

- A device links to one home. A home takes several devices, each under its own name.
- **Restoring the home from a backup.** A device notices when the home's events go back to before what it already received. It then refuses every command with `the home device was restored from an older state` until you run `crosshall link <home>` again. The new link starts a new device store and keeps the old one beside it, named `-rolled-back-<time>`. Sessions on the device join again. Restoring the home also loses whatever the device sent after the backup was made.
- After 7 days the home prunes a device's old receipts, which closes the device's current links; the device reconnects by itself.
- The home and the device must keep a common link version: upgrade both, the home first.
