# D4 — Firecracker upstream report (draft, 2026-07-13)

This file is a ready-to-paste draft for a Firecracker GitHub issue. It is written in
English and anonymized (no host addresses, tokens, or private IPs). The technical
content is a synthesis of a four-round internal investigation; the run log with full
detail is `docs/operations/2026-07-11-cow-burnin-run.md`.

**Framing note:** this is reported as an *elimination result plus a suspected area*, not
a confirmed Firecracker bug. We have not root-caused it inside Firecracker; we have only
shown that it is not fixable from our (guest-image / snapshot-tooling) side and that it
correlates with the snapshot-restore + resume path under host load.

---

## Title

Guest GPF in `inet_bind2_bucket_find` shortly after resuming a diff-snapshot restore, only under heavy concurrent microVM load on ZFS (residual after the v1.16.0 vsock RX-race fix)

## Environment

- Firecracker: **v1.16.1** (also reproduced on v1.15.1 at a much higher rate — see version matrix).
- Host: two independent **x86_64 KVM hosts**, Linux host kernel, **non-ECC RAM** on both.
- Host filesystem: **root-on-ZFS, 128 KiB recordsize**, plus a dedicated 4 KiB-recordsize
  ZFS dataset used for snapshot storage. The failure reproduces regardless of which of
  these two datasets the restore artifacts sit on (verified by relocating them, including
  onto tmpfs).
- Guest kernel: 6.1.x (long-term), single-vCPU or few-vCPU microVMs.
- Guest memory: **≤ 2 GiB (typically 1 GiB), a single memory slot** (no memory hotplug,
  no >3 GiB configuration).
- Workload: many short-lived microVMs spawned/snapshotted/restored/destroyed
  concurrently (a full integration-test gate), i.e. **heavy concurrent restore churn**.

## Symptom

A few seconds after a **diff-snapshot restore is resumed** (~6–9 s of guest uptime, i.e.
shortly after `Resume`), the guest kernel takes a **general protection fault** with the
faulting RIP in **`inet_bind2_bucket_find`** (the IPv4 `bhash2` / inet bind hash), on a
non-canonical pointer, immediately followed by:

```
general protection fault ... RIP: ... inet_bind2_bucket_find ...
Kernel panic - not syncing: Fatal exception in interrupt
```

The rootfs is intact (EXT4 mounts cleanly, no block-I/O errors on the guest console); the
damage is confined to guest **kernel RAM** and always lands in the **inet bind hash**
structure that the network stack touches when the guest agent binds its listening socket
right after resume. The corruption location is **remarkably deterministic** (always
`bhash2`), which argues against purely random bad RAM.

## Minimal reproduction (general shape)

1. Boot a microVM, take a **full snapshot**, then take a **diff snapshot** (single memory
   slot, ≤ 2 GiB).
2. **Restore the diff snapshot** (load base memory + diff, then `Resume`). Our restore
   path reconstructs a merged memory file from base+diff and does a normal full load of
   that merged file; the merged memory file is **byte-verified correct** (see below), so
   from Firecracker's point of view this is a full restore of a correct memory image.
3. Have the guest **bind a listening TCP socket** immediately after resume (any service
   that binds a port on startup will do).
4. Run this **under heavy concurrent restore/spawn load on a ZFS host** so that the
   window between `LoadSnapshot` (paused) and the guest's post-resume execution is
   stretched by host I/O pressure.

Under those conditions the GPF above appears intermittently (see rate below). It does
**not** reproduce with:

- **ext4 host filesystem** (fast restore, small window) — passes.
- **full-snapshot restore** — passes; only the **diff-restore** path fails. The diff path
  is slower (it copies the ~1 GiB base memory image and applies the overlay), so the
  resume-moment contention window is larger.
- **idle / low-load** restores — pass; a large artificial delay before resume also masks
  it (both reduce the contention window).

## What we ruled out (so it is not our side)

- **Loaded memory is correct.** We captured the exact merged memory image a failing
  restore loads and compared it byte-for-byte (SHA) against an idle re-computation:
  **identical**. So the memory Firecracker maps is correct; the corruption happens
  **after load**, in guest kernel RAM, during/just after resume.
- **Durability / cache-coherence of the restore artifact**: fsync of the merged artifact
  before attach, a global `sync()` before resume, `losetup --direct-io=on`, and placing
  the artifact on **tmpfs** (entirely off ZFS) — none eliminate it (tmpfs still failed on
  one host), so the host-FS data path is not the cause.
- **Restore-side dirty-page tracking**: disabling `TrackDirtyPages` on the restored VM —
  no effect.
- **Post-resume guest reconfiguration timing**: delaying the vsock-driven guest IP
  reconfiguration by several seconds after resume — no effect (the guest binds its port
  itself before the delayed reconfig, so it faults first).
- **A stale/overwritten memory file backing a live guest**: paths are per-restore unique
  and unlinked; externally overwriting a live guest's backing memory file did **not**
  corrupt the running guest (Firecracker maps it MAP_PRIVATE/COW), so this mechanism does
  not hold.
- **Known multi-slot diff-corruption fix (#5705, v1.15.0)**: already present in our
  builds, and our VMs are single-slot ≤ 2 GiB, so that condition is not even met.

## Suspected area

We suspect a **residual resume-time race** in the snapshot-restore path, exposed when the
resume window is widened by heavy concurrent load. The strongest single lever we found is
the **v1.16.0 vsock RX-race fix (#5882)** ("RX queue delivering data before the
`TRANSPORT_RESET` ack after restore"), which dropped the failure rate dramatically (see
matrix) — but did not remove it. Because the surviving corruption is always in the inet
bind hash the guest touches right after resume, and is strongly **load/window-correlated**,
we suspect a remaining race around **device/vsock/vCPU resume ordering** rather than
anything in the memory image itself. We have not confirmed this in Firecracker's code and
would appreciate maintainer guidance on where else the restore→resume path could let the
guest observe inconsistent state under load.

## Version matrix

| Firecracker | Diff-restore under heavy ZFS load | Notes |
|---|---|---|
| **v1.15.1** | ~100% failure per gate run | same GPF (`inet_bind2_bucket_find`) |
| **v1.16.1** | ~15–25% failure (intermittent) | **v1.16.0 #5882 vsock RX-race fix is the main improvement**; residual race remains |
| v1.16.1 + large pre-resume delay | lower probability, not eliminated | no fixed delay cleared 2 consecutive clean runs on both hosts |

(ext4 host or full-snapshot restore: 0 failures observed on any version.)

## Ask

- Is a resume-time race around device/vsock/vCPU restore ordering plausible here, beyond
  the v1.16.0 #5882 fix?
- Is there a recommended way to serialize or fully quiesce the guest between
  `LoadSnapshot` and `Resume` so that a slow (I/O-bound) restore cannot let the guest
  resume against partially-restored device state?
- Any guidance on instrumenting the resume path to catch the moment the inet bind hash is
  corrupted would help us confirm or refute the suspected area.
