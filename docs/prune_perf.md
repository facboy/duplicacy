# Where `prune` spends its time

Investigation into the performance of `duplicacy prune`, in the style of
`snapshot_perf.md`, `init_perf.md` and `copy_perf.md`. One issue was worth
fixing and has now been implemented (option 2 below, in `src/duplicacy_filestorage.go`
and `src/duplicacy_chunkoperator.go`); the remaining candidates are recorded for a
follow-up.

## Summary

`prune` is two commands in one. It reads every snapshot of every id, works out
which chunks are no longer referenced, and rewrites the storage to mark those
chunks as fossils (or delete them outright with `-exclusive`). The reading half
is a serial loop over revisions; the chunk half is a multi-threaded chunk
operator that is already parallel.

The reading half is cheap against a local filesystem and expensive against
anything where a read is a round trip. It is also strictly serial: `prune`
accepts `-threads` and uses it for the chunk operator, but the snapshot files
are fetched one at a time regardless, so the flag does nothing for the largest
per-item cost in the command. This is the same shape as the fix `6584fb0` made
for `list`, which added `downloadSnapshots`; `prune` was not updated then and
still calls `downloadSnapshot` directly in its loop
(`src/duplicacy_snapshotmanager.go:2064`).

The chunk half had one real defect. Every metadata chunk it fetched was copied
into the local snapshot cache and read back from there, and that cache write was
a full local-storage write cycle — temp file, `fsync`, `rename`. On a
180-revision repository on ext4 the `fsync` alone turned a real
`prune -r 1-90 -exclusive` from 0.62 s into 0.20 s.

The fix is to keep the cache but stop fsyncing it, for the chunk-cache entries
only. Simply dropping the write is faster still on a fast disk (0.17 s) but loses
more than it gains when a slow mount serves many revisions that share their
metadata, because the cache is what stops those chunks being re-read. Skipping
the `fsync` is only safe where the reader verifies what it finds: a cached
**chunk** is checked against its id on every read and re-fetched from the storage
on a mismatch, so a write torn by a crash is rejected rather than believed.
Cached **snapshot files**, fossil collections and the verified-chunk list are
read back unverified and JSON-parsed, so those keep fsyncing; when their entry is
torn, the first two are reported as errors that stop the command, while the
verified-chunk list is rebuilt by verifying again. See the section below.

`-exhaustive` is the expensive mode, and for a reason that is inherent rather
than accidental: it must expand the chunk sequences of every revision to build
the referenced set before it can walk the chunk tree, so its cost grows with the
number of revisions and with the size of the chunk tree. Non-exhaustive returns
early when there is nothing to delete and does not pay for the expansion at all
unless a snapshot is actually being removed.

## Conclusion

The change worth making is option 2: **keep the cache, but do not `fsync` the
chunk-cache writes**. The defect is real — the chunk operator writes every
metadata chunk to the snapshot cache without checking `IsCacheNeeded()` — but the
write is not pure waste, because the cache is also what stops a metadata chunk
being re-read when many revisions share it. Deleting the write (option 1) is the
fastest choice on a fast disk but the slowest on a slow mount with reuse.
Removing the `fsync` takes out the dominant cost of the write while keeping every
cache hit, and it is the only option that beats HEAD on every fixture measured,
with no case made worse. It was implemented as described below.

The other candidates are worth less:

| Candidate | Scope | Local gain | Cloud gain |
| --- | --- | --- | --- |
| Skip `fsync` on chunk-cache writes | every command that expands a sequence | 3.4-6.5x for `list -files`/`check*`/`prune -exhaustive`; no regression on reuse | none (cache is local by definition) |
| Download snapshot files in parallel under `-threads` | every `prune`, all modes | none (already fast) | one round trip per revision → overlapped |
| Create `fossils/` only when a collection is saved | every `prune` | one `mkdirat` | one round trip |
| Skip the `nesting` probe | every command | one `stat` | one round trip |

`prune` is not like `init`, which is a fixed handful of operations: it scales
with revisions and with the chunk tree, and it is the command a long-lived
repository runs most often. That is why it is worth optimising, and why the two
scaling costs — the serial revision loop and the per-metadata-chunk cache write —
are the ones to look at first.

## The call path

`pruneSnapshots` (`duplicacy/duplicacy_main.go:1168`) parses `-threads`
(`:1178-1181`, default 1), creates the storage with that many threads (`:1188`),
sets up the snapshot cache (`:1225`) and calls `BackupManager.SnapshotManager.
PruneSnapshots` (`:1226`).

`PruneSnapshots` (`src/duplicacy_snapshotmanager.go:1937`) does, in order:

```go
manager.CreateChunkOperator(false, false, threads, false)                  // :1952
...
for _, id := range snapshotIDs {                                           // :2053
    revisions, err = manager.ListSnapshotRevisions(id)                     // :2055
    for _, revision := range revisions {                                   // :2063
        snapshot := manager.downloadSnapshot(id, revision, true, manager.fileChunk, 0)  // :2064  serial
    }
}
...
if toBeDeleted == 0 && !exhaustive { return false }                        // :2321  early out
...
success = manager.pruneSnapshotsExhaustive(...)                            // :2330
        || manager.pruneSnapshotsNonExhaustive(...)                        // :2332
manager.chunkOperator.WaitForCompletion()                                  // :2338
manager.CleanSnapshotCache(latestSnapshot, allSnapshots)                   // :2414-2418
```

The chunk selection and fossilization differ by mode:

- `pruneSnapshotsNonExhaustive` (`:2426`) builds `targetChunks` from the
  snapshots being deleted, then walks the snapshots that are being kept and
  marks the ones that are still referenced. It only calls `GetSnapshotChunks`
  (which downloads the chunk sequences) when a snapshot is flagged for deletion
  (`:2448`) or when a snapshot is kept and there is something to check against
  (`:2463`).
- `pruneSnapshotsExhaustive` (`:2498`) calls `GetSnapshotChunks` for every
  snapshot that is *not* flagged (`:2520`) and then lists the whole chunk tree
  with `ListAllFiles` (`:2529`).

So a non-exhaustive prune with nothing to delete stops at `:2321` after the
snapshot files have been read. An exhaustive prune always expands every
sequence and always walks the chunk tree, which is what makes it the slow mode.

## The revision loop is serial and ignores `-threads`

`prune` creates its chunk operator with `threads` (`:1952`) but downloads the
snapshot files through `downloadSnapshot` one at a time (`:2064`). The
parallel helper that `list` uses, `downloadSnapshots`
(`src/duplicacy_snapshotmanager.go:679`), is right there but is not called from
the prune path. The commit that added it (`6584fb0`) rewrote `list`'s call sites
and left prune's:

```
-			snapshot := manager.downloadSnapshot(id, revision, true)
+			snapshot := manager.downloadSnapshot(id, revision, true, manager.fileChunk, 0)
```

— a signature fix, not a switch to the parallel helper.

Measured on a drvfs mount with 500 revisions and 4 chunks (so the run is almost
all revision reads), `/usr/bin/time`, best of three:

| Build | `-threads 1` | `-threads 8` |
| --- | --- | --- |
| HEAD (`downloadSnapshot` loop) | 0.83 s | 0.84 s |
| prototype using `downloadSnapshots` | 0.85 s | 0.47 s |

The flag has no effect on HEAD and a 1.8x effect once the loop is parallel. On
ext4 the same 500 revisions take 0.02-0.03 s either way, because a revision is
two syscalls and there is nothing to overlap — the same conclusion `list`
reached, which is why that flag also defaults to 1.

A prototype that replaces the loop with

```go
for _, snapshot := range manager.downloadSnapshots(id, revisions, true, threads) { ... }
```

produced byte-identical `prune -exhaustive -dry-run` output on a 60-revision
repository. Note one prerequisite: like `list`, `prune` would have to keep
creating its storage with `threads` (`duplicacy/duplicacy_main.go:1188`), which
it already does.

## The metadata cache is written even when the storage needs no cache

This is the largest finding. `ChunkOperator.DownloadChunk` treated the snapshot
cache as if it were always required:

```go
if task.isMetadata && operator.snapshotCache != nil {                    // :307  no IsCacheNeeded()
    chunk.Reset(true)
    cachedPath, exist, _, err = operator.snapshotCache.FindChunk(...)     // :315
    ...
}
...
if chunk.isMetadata && !chunk.isRawData && len(cachedPath) > 0 {          // :502  no IsCacheNeeded()
    err := operator.snapshotCache.UploadFile(threadIndex, cachedPath, chunk.GetBytes())  // :504
}
```

`FindChunk` returns a path whether or not the chunk was found, so
`len(cachedPath) > 0` is always true when a cache exists: **every** metadata
chunk is written to `.duplicacy/cache/<storage>/chunks/`, and read back before
the storage is consulted. On a cold cache that is one miss plus one write per
metadata chunk. The write at `:502` is the one the fix targets; it is now
`UploadFileNoSync`.

Every other cache access in this area is guarded. `SnapshotManager.downloadFile`
guards the read (`:2721`) and the write-back (`:2764`), and `UploadFile` guards
its write (`:2800`). The `list` investigation implemented exactly this rule for
the snapshot files (`snapshot_perf.md`, candidate #1: "Do not write the snapshot
cache when `IsCacheNeeded()` is false"). The chunk operator was not part of that
change.

`IsCacheNeeded()` is false for local file storage: `CreateStorage` leaves
`isCacheNeeded` off for a plain path and a `flat://` URL, and turns it on only
for the `\\` UNC form and `samba://` (`src/duplicacy_storage.go:258-283`). Every
cloud backend in `src/` returns true. So the affected configuration is a local
or network filesystem that has declared itself cache-free.

### What the unnecessary write costs

A local-storage write is not a single syscall. `FileStorage.UploadFile`
(`src/duplicacy_filestorage.go:150`) does `Lstat`, `MkdirAll` on the parent,
create a randomised `*.tmp`, write, **`fsync`** (`:194`), close and `rename`.
With `strace -f -c` on a 150-revision repository (452 chunks) whose revisions all
differ, so 137 metadata chunks are fetched, `prune -exhaustive -dry-run`:

| Syscall | HEAD | option 1 (guard) |
| --- | --- | --- |
| `fsync` | 137 | 0 |
| `renameat` | 137 | 0 |
| `unlinkat` | 136 | 0 |
| `mkdirat` | 110 | 5 |
| `newfstatat` | 1847 (494 failing) | 986 (11 failing) |
| `openat` | 780 | 539 |
| **total** | **14486** | **9316** |

The cache write counts match the metadata chunk count exactly: 137 `fsync` and
137 `renameat` here, and 284 of each in the 300-revision changing-content fixture
used below (284 metadata chunks fetched). That 1:1 match, rather than the totals,
is what identifies the mechanism.

Wall-clock, best of three:

| Fixture | Mode | HEAD | option 1 (guard) |
| --- | --- | --- | --- |
| ext4, 180 revs, 525 chunks | `prune -r 1-90 -exclusive` | 0.62 s | 0.17 s |
| ext4, 180 revs, 525 chunks | `prune -exhaustive -dry-run` | 0.31 s | 0.04 s |
| ext4, 300 revs, 892 chunks | `prune -exhaustive -dry-run` | 0.89 s | 0.07 s |
| drvfs, 150 revs, 458 chunks | `prune -exhaustive -dry-run` | 1.40 s | 1.01 s |

### `prune` is not the only command affected

`DownloadChunk` is shared, so any command that expands a metadata sequence pays
the same write. On the 180-revision ext4 fixture, `list -files` (which expands
the file, chunk and length sequences of every revision) took 0.55-0.67 s on HEAD
and left 181 files in the cache; with the guard it takes 0.06-0.07 s and leaves
none. `check`, `diff`, `history`, `cat` and `restore` go through the same path.
That makes the guarded version worth more than the `prune` numbers alone
suggest — but it also means the change belongs in `duplicacy_chunkoperator.go`,
where it affects all of them, and it should be validated against those commands
rather than only against prune.

Option 2 was validated command by command on a 150-revision repository with 463
chunks, against HEAD, with a pristine copy of the storage restored before each
pair of runs:

| Command | HEAD | option 2 | Result |
| --- | --- | --- | --- |
| `list -files` | 1.08 s | 0.16 s | 6.75x |
| `check` | 0.47 s | 0.09 s | 5.22x |
| `check -chunks` | 0.54 s | 0.16 s | 3.38x |
| `cat`, `diff` | 0.03 s | 0.02 s | ~1.5x |
| `check -files` | 5.29 s | 4.33 s | 1.22x |
| `list`, `history` | 0.01-0.03 s | 0.01-0.03 s | 1.00x (\*) |

(\*) `list` without `-files`, and `history`, only read the snapshot files and
their chunk sequences, which on this fixture is a handful of chunks; the win is
in the commands that expand a sequence per revision. `check -files` is I/O-bound
on the file contents rather than on the metadata, hence the smaller ratio.

For correctness, each command was run twice (once per build) from the same
pristine storage and the outputs and side effects compared:

- `list`, `list -files`, `list -chunks`, `list -all`, `check`, `check -files`,
  `check -stats`, `diff`, `cat` (of a snapshot and of a file) and `history`:
  identical stdout, stderr and exit code, once the throughput/time fields in the
  progress lines are normalised away.
- `check -chunks`: the *set* of chunk hashes verified is identical (463 of 463).
  The line order differs, but HEAD against itself also differs — the verification
  is parallel, so ordering was never deterministic and is not an option-2 effect.
- `restore -r 30` into the same target path: identical output and all 46 restored
  files byte-identical by `sha256sum`, confirming that skipping the cache fsync
  does not feed anything different back into restore.
- The cache is still populated and still reused: after `list -files`, both builds
  leave 329 files in the cache, and a second warm-cache `list -files` takes 0.11 s
  on both. Option 2 changes only when the cache bytes reach the disk.

### Three ways to fix it, and which one to pick

The defect admits three fixes. All three leave the storage byte-identical.

1. **Guard the cache with `IsCacheNeeded()`** (`duplicacy_chunkoperator.go:307`
   and `:502`), matching every other cache access in the code. Fastest on a fast
   disk. It loses badly, however, when one slow mount serves many revisions that
   share their metadata: with the cache gone, the shared chunks are re-read for
   every revision. **Not implemented.**
2. **Keep the cache, but don't `fsync` the chunk-cache writes.** `UploadFile`
   fsyncs every write (`src/duplicacy_filestorage.go:209`), which is the
   expensive part of the cycle; the cache write is a rename-into-place whose only
   requirement is to survive this process. This keeps every cache hit *and*
   removes the dominant cost. **Implemented** (see below).
3. **Split "needs a cache" from "is local"**, so that a mount which is local but
   slow can get the reuse without the fsync. The most correct but the largest
   change, since `IsCacheNeeded()` is a storage property consulted in several
   places. **Not implemented.**

Measured against each other, best of three, `/usr/bin/time`:

| Fixture | HEAD | 1. guard | 2. no `fsync` |
| --- | --- | --- | --- |
| ext4, 180 revs, churn, delete 90 | 0.62 s | 0.17 s | 0.20 s |
| ext4, 150 revs, churn, `-exhaustive` | 0.52 s | 0.06 s | 0.10 s |
| drvfs, 150 revs, churn, `-exhaustive` | 1.44 s | 0.95 s | 1.03 s |
| drvfs, 300 revs, 4 chunks, reuse, `-exhaustive` | 0.56 s | 1.16 s | 0.53 s |

Option 2 is never the slowest and is best or near-best everywhere; the reason
option 1 wins the two fast-disk rows is that it skips the write entirely, at the
cost of more than doubling the run on the reuse row. **Option 2 is the
recommendation**: the only one that improves every measured case, and the
smallest change — one flag on `FileStorage`, set for the snapshot cache where it
is created (`src/duplicacy_backupmanager.go:105`).

Option 2 is a durability question as much as a performance one. It is safe only
where the reader verifies the cache entry before trusting it, and that is true of
the chunk cache alone: `DownloadChunk` re-derives the id of the chunk it loads and
falls back to the storage when it does not match, so a torn write is rejected and
re-fetched. The cached **snapshot files**, **fossil collections** and
**verified-chunk list** are read back unverified and JSON-parsed, so a torn entry
there would be fatal — `list` and `prune` failed outright in a test that truncated
one. Those writes therefore keep fsyncing.

#### How it was implemented

- `FileStorage.UploadFile` keeps fsyncing; a new `FileStorage.UploadFileNoSync`
  goes through the same code with the `fsync` skipped
  (`src/duplicacy_filestorage.go`). The default is unchanged, so every other
  writer — the storage itself, the snapshot-file cache, the fossil collections —
  is untouched.
- The two chunk-cache writers call `UploadFileNoSync`: the one in `DownloadChunk`
  (one per metadata chunk, the write that dominates) and the one in `UploadChunk`.
- Measured end to end with the built binary on a 150-revision repository with 486
  chunks: `prune -exhaustive` 0.65 s → 0.12 s (5.4x), `list -files` 1.57 s →
  0.24 s (6.5x), `check` 0.65 s → 0.12 s (5.4x), `check -chunks` 0.78 s → 0.23 s
  (3.4x). `syscall fsync` per `prune -exhaustive` fell from 142 to 0 while
  `renameat` stayed at 142, which is the signature: the cache is still written and
  renamed into place, only the flush is gone. On a storage that needs a cache
  (`samba://`), where snapshot files are cached too, the count went 6 → 3: the
  chunk-cache fsyncs are gone and the snapshot-file ones remain.
- Correctness: `prune -exhaustive -dry-run` output is identical, a real
  `prune -r 1-75 -exclusive` produces a byte-identical storage tree, and a
  `restore` of all 151 files matches by `sha256sum`.

### Correctness

The implemented change was verified in three ways.

- **Against HEAD, command by command**, from the same pristine storage copy:
  identical stdout, stderr and exit code for `list`, `list -files`,
  `list -chunks`, `list -all`, `check`, `check -files`, `check -stats`, `diff`,
  `cat` (snapshot and file) and `history`; the same 463-element set of verified
  chunk hashes for `check -chunks`; and a `restore` whose 46 files are
  byte-identical by `sha256sum`.
- **At the storage level**: a real `prune -r 1-75 -exclusive` produces a storage
  tree that `diff -rq` reports identical, and `prune -exhaustive -dry-run` prints
  byte-identical output. Expected, since skipping the `fsync` changes when bytes
  reach the disk, not which bytes are written.
- **The safety property the change depends on**: a truncated chunk-cache entry is
  rejected and the chunk re-fetched from the storage. `TestCorruptCachedChunkIsRefetched`
  pins this down — it fails if the id check in `DownloadChunk` is removed — and
  `TestSnapshotCacheSkipsSync` checks that the cache is still written and readable.
  `TestPruneSingleRepository` and `TestPruneSingleHost`
  (`src/duplicacy_snapshotmanager_test.go:899`, `:943`) exercise the prune deletion
  path with and without `exclusive`, tagged snapshots and retention policies, and
  `TestCorruptNonChunkCacheEntries` records the read-side behaviour of the three
  cache entries that are *not* covered by the no-fsync change.

What the change gives up is that a chunk-cache entry is no longer guaranteed to
survive a crash. That is the whole point of the trade: the entry lost is
re-derived from the storage on the next read. The cached snapshot files and
fossil collections keep their `fsync`, because they are read back unverified.

## `-exhaustive` expands every sequence

The two modes cost very differently, and the difference is the sequence
expansion. On ext4, best of three, `-dry-run` (read everything, no deletion):

| Fixture | Revisions | Chunks | Non-exhaustive | `-exhaustive` |
| --- | --- | --- | --- | --- |
| unchanged content | 300 | 8 | 0.03 s | 0.07 s |
| changing content | 300 | 902 | 0.03 s | 0.87 s |

With `-d`, the two fixtures differ in exactly one respect that matters: the
changing-content run fetches and writes back 284 metadata chunks, while the
unchanged-content run fetches a single one and serves the other 299 revisions
from the cache. That is the whole difference. The tree walk does grow with the
chunk tree — with `-v`, `LIST_FILES` ("Listing chunks/...") appears 424 times for
the 902-chunk fixture against 11 times for the 8-chunk one, since
`pruneSnapshotsExhaustive` (`:2529`) lists the top directory and every
subdirectory — but it is a small part of the total next to the per-sequence
fetches, and it is the same walk `-delete-only` performs.

Consequences:

- `-exhaustive` cannot take the `:2321` early exit, by design: it has to expand
  the sequences to know what is referenced before it can find dangling chunks
  and temporary files. Scheduling it less often (the wiki recommends it
  periodically) is the practical lever.
- The `-threads` flag does not change any of this. The sequence chunks are
  fetched by the operator, but each `DownloadSequence` waits for its chunks one
  at a time (see the smaller items), and the tree walk runs on the calling
  goroutine. A `-threads 1/4/16` sweep on the 300-revision ext4 fixture was flat
  (0.94 s / 0.94 s / 0.95 s).
- The non-exhaustive mode with deletions does pay the expansion too, because it
  calls `GetSnapshotChunks` on the snapshots being deleted and on the ones being
  kept to compare against. The 0.62 s delete above is mostly that.

## Smaller items

- **`fossils/` is created unconditionally.** `PruneSnapshots` calls
  `manager.snapshotCache.CreateDirectory(0, "fossils")` at `:2078` on every run,
  including one that ends at the `:2321` early return and creates no collection.
  `FileStorage.CreateDirectory` is an `os.Mkdir` whose `EEXIST` is swallowed
  (`src/duplicacy_filestorage.go:108-115`), so locally it is a wasted `mkdirat`
  (`mkdirat` count 5 with the guard); on a directory-based backend it is a
  `Stat` + `Mkdir` pair per run.
- **The `nesting` probe is paid by every command, prune included.** `prune`
  creates the storage at `duplicacy/duplicacy_main.go:1188` and
  `CreateBackupManager` downloads the config, which ends in
  `storage.SetNestingLevels(config)` (`src/duplicacy_config.go:476`) and probes
  for a file named `nesting` that nothing writes
  (`src/duplicacy_storage.go:109-126`). This is the same item recorded as a
  candidate in `init_perf.md`; it is one round trip per run.
- **`CleanSnapshotCache` probes every chunk of every cached snapshot.** At the
  end of a successful prune, each cached snapshot file is read and each of its
  chunks is looked up first in the cache and then in the storage
  (`src/duplicacy_snapshotmanager.go:443-444`). On a 60-revision fixture with the
  cache enabled, `prune -r 1 -exclusive` issues 290 `newfstatat` calls under the
  cache's `chunks/` and 46 under the storage's. It scales with the size of the
  cache rather than with the repository, so it costs most on a cache that has
  not been cleaned before.
- **`DownloadSequence` expands a sequence one chunk at a time.** It calls
  `CreateChunkOperator(false, false, 1, false)` (`:315`) and then, per chunk,
  `manager.chunkOperator.Download(chunkHash, 0, true)` (`:317`), which blocks on
  the chunk's completion channel (`src/duplicacy_chunkoperator.go:155-163`). So
  a sequence is fetched serially even when the operator was created with more
  threads — in `PruneSnapshots` it is, at `:1952`, and `CreateChunkOperator`
  only creates the operator if none exists (`:307-311`), so the `1` here is
  ignored. On a cloud storage each metadata chunk of a sequence is therefore a
  round trip taken one at a time.
- **`prune` with a tag or a retention policy downloads every revision anyway.**
  The tag filter (`:2310`) and the retention policy (`:2259`) are applied after
  the snapshot files have been downloaded, which is unavoidable for the
  retention policy (it needs the timestamps) but not for `-t`, which only needs
  `snapshot.Tag`. This is a marginal item: the filter is applied per id, so the
  number of ids bounds the saving, and a tag filter is not a common invocation.

### Deliberately not pursued

- **A chunk index or a per-revision index.** As in `snapshot_perf.md`
  (candidate #7) and `copy_perf.md`, this would change the storage format on
  disk. Prune's chunk walk is what an index would replace, but it would have to
  be maintained by every writer and understood by old clients.
- **Parallelising the tree walk.** `ListAllFiles` is a serial BFS
  (`src/duplicacy_snapshotmanager.go:555-590`). It is proportional to the number
  of chunk directories, and the measurements above put it well behind the
  sequence expansion; parallelising it would change the order chunks are
  fossilized in, which is user-visible in the log.
- **Skipping the sequence expansion in non-exhaustive mode when `-exclusive` is
  given.** The snapshots being deleted still have to be expanded to know which
  chunks to fossilize, so there is nothing to skip.
- **The `fsync` in `FileStorage.UploadFile`.** It is a deliberate durability
  guarantee and applies to the real chunk uploads as well as the cache; the
  cache-specific question is covered above.
- **`-exclusive` versus fossil collection.** `-exclusive` deletes chunks instead
  of renaming them to `.fsl`, which skips the fossil-collection machinery, but
  it also requires `IsMoveFileImplemented()` at
  `duplicacy/duplicacy_main.go:1216` and is not a speed switch: the measurements
  above use it only to make the deletions deterministic between runs.

## How to confirm on a given setup

- `duplicacy -d prune ...` sets DEBUG logging
  (`duplicacy/duplicacy_main.go:146-148`); `-v` sets TRACE. The `DOWNLOAD_FILE`
  lines ("Downloaded file snapshots/<id>/<n>") show the serial revision loop —
  they are printed one after another with no overlap, and their count is the
  revision count. Compare with `duplicacy -d list` on the same repository.
- The `CHUNK_DOWNLOAD` ("Chunk ... has been downloaded") and `CHUNK_CACHE`
  ("loaded from the snapshot cache") counts show the metadata fetches. On a
  local FileStorage the cache hits *and* the cache writes should not happen; if
  they do, the unguarded write is active.
- `duplicacy -d prune ...` prints `SNAPSHOT_DELETE` per revision being removed
  and, for the fossils, `FOSSIL_COLLECT`/`FOSSIL_DELETABLE`. The gap between the
  last `DOWNLOAD_FILE` and the first of those is the chunk-selection phase.
- `strace -f -c -e trace=fsync,renameat,unlinkat,mkdirat,newfstatat,openat
  duplicacy prune ...` and look at `fsync` + `renameat`: one pair per metadata
  chunk is the cache write-back. Option 1 removes both (zero of each); option 2
  keeps the `renameat` and removes the `fsync`; that pair is the signature to look
  for when confirming option 2.
- Compare the same repository with the storage on a native filesystem and on the
  mount in question. The syscall counts match and the timings differ, which
  separates filesystem cost from the command's own behaviour.
- `prune -exhaustive` against `prune` on the same repository sizes the two modes;
  `prune -r N` (a single revision) gives the marginal per-revision cost. Note
  that `-delete-only` returns at `:2224`, before the chunk-selection phase, so it
  measures the fossil-collection cleanup only rather than the work described
  above.
- Run `go test ./src/ -run 'TestPrune' -vet=off -v` for the deletion semantics,
  `go test ./src/ -run 'TestCorruptCachedChunkIsRefetched|TestSnapshotCacheSkipsSync|TestCorruptNonChunkCacheEntries'
  -vet=off -v` for the cache behaviour, and `integration_tests/copy_test.sh` and
  `integration_tests/test.sh`, which run `prune -exhaustive -exclusive` and a
  plain `prune` and check the storages afterwards.
- To check either option does not change results, prune the same pristine copy
  of a storage with and without it and compare the trees with `diff -rq`; that is
  how the claims above were verified.
