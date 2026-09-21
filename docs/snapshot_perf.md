# Why listing snapshot revisions is slow

Investigation into the performance of `duplicacy list`. This document records
the findings and the candidate fixes; candidate fixes #1, #2 and #3 below have
since been implemented in `src/duplicacy_snapshotmanager.go`.

## Summary

`duplicacy list` is a serial, one-operation-at-a-time loop. For every revision of
every snapshot id it performs an existence check and a file download, with a
chunk operator hard-coded to a single thread and a snapshot cache that is
written but never read.

Against cloud storage each of those operations is a network round trip, so the
cost grows linearly at roughly two round trips per revision; fix #3 removed the
existence check when the revision was just listed, halving that.

Against local storage the same code runs, but each hop is a syscall instead of a
round trip: it is usually fine on a fast native filesystem (~3 ms per revision)
and painful on a slow local mount.

## The call path

`listSnapshots` (`duplicacy/duplicacy_main.go:899-950`) creates the storage,
calls `CreateBackupManager` (which downloads the config), calls
`SetupSnapshotCache`, and then hands off to `SnapshotManager.ListSnapshots`
(`src/duplicacy_snapshotmanager.go:657-765`).

`ListSnapshots` is a plain nested loop with no concurrency:

```go
manager.CreateChunkOperator(false, false, 1, false)   // threads == 1
...
for _, snapshotID = range snapshotIDs {
    revisions, err = manager.ListSnapshotRevisions(snapshotID)
    for _, revision := range revisions {
        snapshot := manager.DownloadSnapshot(snapshotID, revision)
        ...
    }
}
```

`revisionsToList` is empty unless `-r` was passed, so `ListSnapshotRevisions`
(`src/duplicacy_snapshotmanager.go:484-507`) always runs and always hits the
remote storage. The local snapshot cache is never consulted for the revision
list.

## Cost per revision

`DownloadSnapshot` (`src/duplicacy_snapshotmanager.go:209-242`) used to do two
operations for every revision, including revisions already present in the local
cache:

1. `manager.storage.GetFileInfo(0, snapshotPath)` — an unconditional existence
   check. The comment at `:214` explains this is deliberate, because the cache
   may hold a stale copy, but it costs a round trip per revision even when the
   file is already cached (`src/duplicacy_sftpstorage.go:242-263`,
   `src/duplicacy_b2storage.go:177-202`,
   `src/duplicacy_gcdstorage.go:741-772`). Fix #3 skips it when the revision
   came from `ListSnapshotRevisions`, which already enumerated the directory.
2. `manager.DownloadFile(snapshotPath, snapshotPath)` — a cache lookup that
   misses on the first run, then the actual download
   (`src/duplicacy_snapshotmanager.go:2610-2665`).

So the default `list` used to be approximately:

```
1 x ListFiles("snapshots/<id>/")
+ for each revision: (GetFileInfo + DownloadFile)
```

and is now:

```
1 x ListFiles("snapshots/<id>/")
+ for each revision: DownloadFile
```

Until fix #2 was implemented there was also a
`manager.storage.CreateDirectory(0, snapshotDir)` (plus the equivalent call on
the snapshot cache) at the top of `DownloadSnapshot`, and the same pair at the
top of `ListSnapshotRevisions`. Both are gone now; see the candidate fixes below
for why that was also a correctness fix and not only a performance one.

At 50-100 ms RTT and a few hundred revisions this is tens of seconds. With
`-all` it repeats for every snapshot id after a single `snapshots/` listing.

## The snapshot cache is written but never read

`SnapshotManager.DownloadFile` guards the cache **read** with `IsCacheNeeded()`:

```go
if manager.storage.IsCacheNeeded() {
    ... manager.snapshotCache.DownloadFile(0, path, manager.fileChunk) ...
}
```

but the cache **write** at the end is unconditional
(`src/duplicacy_snapshotmanager.go:2669`):

```go
err = manager.snapshotCache.UploadFile(0, path, manager.fileChunk.GetBytes())
```

For a local path, `CreateStorage` passes `isCacheNeeded = false`
(`src/duplicacy_storage.go:237-244`), so `FileStorage.IsCacheNeeded()` returns
false and the cache is never read. Yet every `list` still copies each snapshot
file into `.duplicacy/cache/<name>/snapshots/<id>/`, and
`FileStorage.UploadFile` performs a temp-file write, an `fsync` and a `rename`
for each one (`src/duplicacy_filestorage.go:150-220`).

Verified directly:

- Corrupting a cached snapshot file and running `list -r 5` still returns the
  correct snapshot — the cache is not read.
- Deleting `.duplicacy/cache/default` and running `list` recreates all 301
  snapshot files.
- Warm and cold-cache timings are indistinguishable because the cache is
  write-only.

The cache therefore does not reduce round trips on any backend; it only removes
the payload transfer for backends where it is actually consulted.

## Local storage measurements

Measured with a binary built from HEAD against a real repository with 301
revisions.

| Storage location | `list` (301 revisions) | per revision |
| --- | --- | --- |
| `/tmp` (native ext4) | 0.89 s warm, 0.96 s cold | ~3.0 ms |
| `/mnt/d` (WSL drvfs, still local) | 12.0 s | ~40 ms |

Scaling on ext4 with `list -r ...` confirms it is linear, with no fixed overhead
beyond roughly 10 ms of startup:

```
1 rev    0.03 s
1-10     0.06 s
1-50     0.17 s
1-100    0.31 s
1-200    0.60 s
1-301    0.88 s
```

Scaling on drvfs:

```
1 rev    0.10 s
1-10     0.43 s
1-50     1.98 s
1-100    3.84 s
1-200    7.83 s
1-301   12.13 s
```

Other commands, same repository:

```
                 ext4      drvfs
list             0.89 s    12.0 s
list -all        1.08 s    12.2 s
list -files      0.99 s    27.9 s
list -chunks     0.94 s      n/a
```

`list -files` nearly triples the time on a slow mount because it adds the
per-revision metadata-chunk reads.

The process is idle, not computing: 6% CPU, 66,009 voluntary context switches
and 12.08 s elapsed for `list` on drvfs.

### Syscall attribution on drvfs

Summing per-call wall time from `strace -f -T`:

| syscall | total | calls | per call |
| --- | --- | --- | --- |
| `fsync` | 4.19 s | 301 | 13.9 ms |
| `newfstatat` | 2.73 s | 1209 | 2.3 ms |
| `renameat` | 1.65 s | 301 | 5.5 ms |
| `openat` | 1.51 s | 622 | 2.4 ms |
| `mkdirat` | 1.24 s | 605 | 2.0 ms |

11.36 s of the 12.0 s is spent inside these filesystem calls. The counts are
identical on ext4 — only the per-call latency differs, which is why the same
code is roughly 13 times slower on drvfs. `fsync` alone is more than a third of
the total.

Per revision the loop emits: 4 `newfstatat`, 2 `openat` and, before fixes #1
and #2, 2 `mkdirat`, 1 `fsync` and 1 `renameat`.

The per-revision cost broke down as (line numbers as of the original
investigation, before fix #1 and fix #2):

```
DownloadSnapshot:
  storage.CreateDirectory  -> mkdirat (EEXIST)                    :213-214
  storage.GetFileInfo      -> newfstatat                           :220
  DownloadFile:
    cache read (SKIPPED, IsCacheNeeded() == false)                 :2626
    storage.DownloadFile   -> openat + read                        :2636
    snapshotCache.UploadFile -> openat + write + fsync + renameat  :2669
```

After fix #1 removed the cache write and fix #2 removed the `CreateDirectory`
calls, only `GetFileInfo` and the download remain:

```
DownloadSnapshot:
  storage.GetFileInfo      -> newfstatat                           :216
  DownloadFile:
    cache read (SKIPPED, IsCacheNeeded() == false)                 :2612
    storage.DownloadFile   -> openat + read                        :2622
```

Measured on the same 20-revision repository: `mkdirat` went from 44 to 2 calls
per `list`, and the cached snapshot files written went from 20 to 0.

After fix #3 the `GetFileInfo` is also gone for revisions that were just listed
by `ListSnapshotRevisions`:

```
ListSnapshotRevisions:
  storage.ListFiles("snapshots/<id>/") -> openat + read            :500
DownloadSnapshot (revision was listed, so no GetFileInfo):
  DownloadFile:
    cache read (SKIPPED, IsCacheNeeded() == false)                 :2627
    storage.DownloadFile   -> openat + read                        :2637
```

`DownloadSnapshot` still calls `GetFileInfo` for a revision the user named
explicitly with `-r`, because that revision was not enumerated and the check
protects against a stale snapshot cache entry.

Measured on a 60-revision local repository: `newfstatat` per `list` fell from 77
to 37 (one per revision), and on a slow drvfs mount `/usr/bin/time` went from
0.40 s to 0.27 s. On native ext4 the same repository is ~0.02 s either way,
where the syscall is a few microseconds.

## `list -files` and `list -chunks`

With `showFiles` (`src/duplicacy_snapshotmanager.go:713-751`) each revision gets:

- `DownloadSnapshotSequences` -> `DownloadSequence` -> `chunkOperator.Download`
  per metadata chunk (`src/duplicacy_snapshotmanager.go:339-345`). The operator
  has one worker, and every chunk download is a `FindChunk` followed by a
  `DownloadFile` (`src/duplicacy_chunkoperator.go:284-490`) — two serial
  operations per metadata chunk, per revision.
- `snapshot.ListRemoteFiles(...)` called twice (`:727` and `:742`), each
  re-iterating and re-decoding the whole file sequence
  (`src/duplicacy_snapshot.go:107-207`). The second pass is served from the
  cache, but the first is still fully remote.

Against local storage these are a couple of syscalls rather than network round
trips, so they are proportionally cheaper than in the cloud case but still
strictly additive.

## Backend-specific amplifiers

- **SFTP / SFTP-C** (`src/duplicacy_sftpstorage.go`): `ReadDir`, `Stat` + `Mkdir`
  and `Open` are all sequential on one multiplexed connection, and every call is
  wrapped in `retry()` which can reconnect and back off (`:135-165`). Worst
  ratio of round trips to payload.
- **GCD** (`src/duplicacy_gcdstorage.go`): path-to-id resolution uses
  `listByName` name queries. Before fix #2, `CreateDirectory` -> `GetFileInfo` ->
  `getIDFromPath`/`listByName`, and `ListFiles("snapshots/<id>/")` ->
  `getIDFromPath` again, turned the operations above into several Drive API
  queries per revision, plus Drive quota.
- **B2** (`src/duplicacy_b2storage.go:41-92` and
  `src/duplicacy_b2client.go:397-539`): the `snapshots` listing is a flat prefix
  scan with `maxFileCount = 1000` that pages through every snapshot file of
  every id and revision, deduplicating subdirectories client-side. `list -all`
  on a large bucket pays for a full `snapshots/` traversal.
- **S3, Azure, GCS, Swift, WebDAV**: cheaper for the id listing (delimiter or
  CommonPrefixes) but still pay the per-revision `GetFileInfo` and
  `DownloadFile`.
- **OneDrive, Hubic**: `ListEntries` is non-recursive with 1000-item paging, so
  the id listing is fine.

## How to confirm on a given setup

- `duplicacy list -v` sets TRACE logging
  (`duplicacy/duplicacy_main.go:142-148`), printing the `LIST_FILES`,
  `SNAPSHOT_LIST_REVISIONS` and `DOWNLOAD_FILE` sequence; `-d` adds DEBUG.
  Timing the gap between consecutive `SNAPSHOT_INFO` lines shows whether the
  cost is the revision listing or the per-revision download.
- `/usr/bin/time -v duplicacy list`: if CPU percentage is low and elapsed is
  high, the time is in syscalls or round trips, not processing.
- `strace -f -c -T -e trace=fsync,renameat,openat,newfstatat,mkdirat duplicacy
  list` and sum the per-call times. If `fsync` dominates, it is the write-only
  snapshot cache.
- `list -r 1` versus a full `list` gives the per-revision marginal cost.
- Compare the same repository on a native filesystem against the mount in
  question: syscall counts will match while timings differ.

## Candidate fixes

Ordered roughly by expected benefit for local storage.

- **Do not write the snapshot cache when `IsCacheNeeded()` is false.** Mirror the
  read-side guard at `src/duplicacy_snapshotmanager.go:2669`. This removes the
  `fsync`/`renameat` pair and the cache `mkdirat`/`newfstatat` traffic per
  revision, which is the majority of the slow-mount cost. **Implemented**: the
  cache write in `DownloadFile` is now guarded by `IsCacheNeeded()`, matching the
  cache read and `UploadFile`.
- **Skip the redundant `CreateDirectory` on storage and cache** in
  `DownloadSnapshot` (`:213-214`) and `ListSnapshotRevisions` (`:494-499`). Each
  is an `EEXIST` `mkdirat` or a `Stat` round trip, and on a directory-based
  backend it has a visible side effect: listing a snapshot id that does not
  exist *creates* `snapshots/<id>`, which then shows up as a repository in
  `duplicacy info`, in `check -all` output and in `prune`. It also makes read
  commands fail on read-only storage, because `ListSnapshotRevisions` treats a
  failed `CreateDirectory` as fatal. **Implemented**: both calls are gone. The
  directory is created by `SnapshotManager.UploadFile` instead, which is the
  only place that needs it (Dropbox and similar backends cannot upload a file
  into a directory that does not exist). A missing snapshot directory now reads
  as an empty one: `FileStorage`, `SambaStorage` and `ACDStorage` already
  behaved that way, and `SFTPStorage`, `GCDStorage`, `DropboxStorage`,
  `WebDAVStorage`, `OneDriveStorage` and `HubicStorage` were changed to match.
- **Skip the `GetFileInfo` existence check** when the snapshot body is already
  cached, or fold it into the directory listing that `ListSnapshotRevisions`
  just performed. **Implemented**: `ListSnapshotRevisions` already enumerated the
  snapshot directory, so the revisions it returns are known to exist and
  `ListSnapshots`, `CheckSnapshots`, `ShowHistory`, `downloadLatestSnapshot`,
  `PruneSnapshots` and `CopySnapshots` all download them through the private
  `downloadSnapshot(..., listed = true)`, which skips the check. `DownloadSnapshot`
  keeps it, so a revision named with `-r` is still validated against the storage
  (the cache may hold a stale copy), and `copy` keeps its own destination check.
- **Parallelise the revision loop** and raise the chunk operator thread count for
  `list`; revisions are independent. This is the only change that helps when the
  filesystem itself is the bottleneck.
- **Avoid the double `ListRemoteFiles` pass** in the `showFiles` branch by
  computing sizes and printing in a single traversal.
- **Let backends list only direct children of `snapshots/`** (B2 and other
  prefix-based backends) instead of scanning the whole subtree.
- **Introduce a lightweight revision index** instead of one file per revision, so
  listing a repository becomes a single object read.
