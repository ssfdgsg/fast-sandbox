# Native snapshot local prewarm — review branch

This implements the native path of issue #57 on the current checkout.
Enable it with `FAST_SANDBOX_SNAPSHOT_PREWARM=true` in the Fastlet process.
The default is disabled. No API, CRD or scheduling change is required.

## Behavior

After the VM has resumed and remote publication has succeeded, the Driver
attempts to install staging under `images/sha256(templateName)`. The cache
uses the same manifest and file-name mapping as Agent pull: `rootfs.ext4`
becomes `rootfs.img`; VM state and memory keep their names.

The staged manifest must match the digest returned by publication. File
types and logical sizes must match the manifest. The Driver trusts the
immutable staging bytes it already hashed during manifest assembly; it does
not read all artifact bytes again. Files move into a private directory on
the cache filesystem, then Linux `RENAME_NOREPLACE` exposes the complete set.
This branch skips cross-filesystem moves rather than adding a copy fallback.

Publication remains the success boundary. Installation consumes staging
whether it succeeds, skips or fails. A structured log reports outcome,
reference, snapshot ID, kind, artifact size and elapsed time:

| Outcome | Meaning |
| --- | --- |
| `committed` | Complete directory installed |
| `already_cached` | Matching manifest digest and complete file sizes |
| `skipped_busy` | Another cache reader/writer owns the entry |
| `skipped_conflict` | Existing entry has different content or is incomplete |
| `skipped_capacity` | Logical cache quota or metadata space gate failed |
| `skipped_filesystem` | Cross-filesystem move or atomic installation unsupported |
| `failed` | Validation or filesystem error; publication remains successful |

Use a new template name for every version. Matching manifest digest plus file
sizes does not detect same-size corruption of an existing cache. Agent pull
continues to perform full checksum verification. Alias refresh and global
template-name uniqueness are outside this implementation.

## Concurrent access

Driver and Agent share advisory `flock` files in
`<StateRoot>/image-locks/<reference-hash>.lock`. Lock files are never deleted,
including during cache eviction, so every process continues locking the
same inode. Deploy the updated Driver and Agent together. Native runtime
support already targets Linux; atomic installation requires filesystem
support for `RENAME_NOREPLACE`.

| Operation | Lock lifetime |
| --- | --- |
| Agent pull / repair | Exclusive, context-cancellable wait; check cache again after acquiring |
| Snapshot cache installation | Exclusive, nonblocking; busy means skip |
| Driver restore | Shared, nonblocking acquisition; held through VM restore and durable state creation |
| Agent cache inspection | Shared, nonblocking acquisition; busy means not ready |
| GC | Exclusive, nonblocking; rescan durable references before unlinking |

GC derives the effective restore reference from Sandbox state, including
checkpoint restores. It counts every regular file in each native cache entry,
including memory, VM state and metadata. Inventory excludes private staging
directories. The logical quota is an admission hint during prewarm and is
enforced by periodic GC; concurrent writers can temporarily exceed it.
Available filesystem blocks are checked separately, with a 1 MiB metadata
reserve. Neither number estimates physical usage of shared reflink extents.

## Verification and remaining boundaries

Automated coverage includes:

- Prewarm on/off, remote success despite cache capacity skip, and restore
  through the Driver using the fake Firecracker API.
- Agent acceptance of the installed manifest/checksums and zero HTTP requests
  on a warm pull; after GC eviction, the same reference cold-pulls successfully.
- Independent writable instance rootfs files and inode-preserving installation.
- Busy/conflicting cache, truncated staging, digest mismatch, cancelled work
  and staging cleanup.
- An active pull racing with prewarm and GC, cancellable competing pulls,
  active-reader protection, checkpoint reference protection, and cross-process
  file-lock exclusion.

This is a cache optimization. Directory rename provides atomic visibility,
not power-loss durability. This branch preserves existing hand-seeded local
cache support, including entries without a full manifest. The Driver checks
published manifest file sizes without adding a full checksum scan to each
hot restore; it does not implement generic restore-error repair/retry.
Agent journal pins/leases are not newly integrated into Driver GC.

Real Firecracker/KVM recovery and multi-GiB latency/space measurements remain
manual validation. Measure publication overhead and subsequent restore time
on the target filesystem, and verify artifact GET bytes. Start by pinning the
new Sandbox to the publishing node. Automatic cache-affinity scheduling is
a separate change; local cache installation alone does not guarantee placement
or retention. OCI artifacts and extra artifact types on newer upstream branches
require their own integration.
