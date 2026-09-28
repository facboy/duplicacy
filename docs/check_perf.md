# Where `check` spends its time

Investigation into the performance of `duplicacy check`, in the style of
`snapshot_perf.md`, `copy_perf.md`, `prune_perf.md` and `init_perf.md`. One real
defect was found — the revision loop ignored `-threads`, exactly as prune's did
before `c651bb1` — and it has been fixed: the loop now reads its snapshot files
through `downloadSnapshots`, the same helper `list` and `prune` use. The
`check -files` mode is the expensive one, and its cost is inherent to what the
mode promises rather than to a mistake.

## Summary

`check` is three phases:

1. a read of the `snapshots/` directory per id and one snapshot file per
   revision, to build `snapshotMap`;
2. a walk of the whole `chunks/` subtree, to build `chunkSizeMap`;
3. per revision, either a referenced-chunk existence check, or the file
   verification of `-files`.

The first phase is where the defect was, and it is fixed. `CheckSnapshots`
(`src/duplicacy_snapshotmanager.go:962`) read the snapshot files one at a time on
the calling goroutine although it already accepted `-threads` and spends it on
the chunk operator (`:965`). `list` and `prune` both read their revisions through
`downloadSnapshots` (`:809`) under `-threads`; `check` was simply never switched
over, so the flag was inert for the largest per-item cost of the command's first
phase. It now calls the helper (`:1039`). Measured on a virtiofs mount whose run
is almost all revision reads, the flag is flat on HEAD and worth about 1.2x once
the loop is parallel, with `-threads 1` unchanged.

The second phase is a `ListAllFiles` of the entire chunk tree (`:986`), which is
proportional to the number of chunk directories rather than to the revisions
being checked. It is paid in full even by `check -r 5`, and its result
(`chunkSizeMap`) is only ever consulted for the chunks the named revisions
reference.

The third phase is proportional to the data. The plain `check` only compares the
referenced chunk ids against the listing, so it does not read a chunk at all;
`-chunks` downloads every referenced chunk to verify its hash and does use
`-threads`; `-files` reads every file chunk, on one goroutine, and is the mode
that costs seconds to minutes.

## Conclusion

**The one fix that was worth making has been made.** The revision loop reads its
snapshot files through `downloadSnapshots`, so `-threads` reaches the
per-revision read. It is the change already applied to `prune` in `c651bb1` and
to `list` in `6584fb0`: a few lines, byte-identical to the serial loop at one
thread and to HEAD's output at every thread count, and documented below.

Nothing else is a defect:

- `-chunks` already overlaps its verification, because `CheckSnapshots` creates
  the chunk operator with the user's `-threads` (`:965`) and the verification
  loop submits through it (`:1265`). On a 20-revision, 200 MB fixture `-chunks`
  goes from 2.05 s at one thread to 0.22 s at eight.
- `-files` is I/O-bound on the file chunks themselves, and `VerifySnapshot`
  (`:1507`) hands those to `RetrieveFile` (`:1552`), which blocks on the calling
  goroutine. Raising `-threads` therefore changes nothing measurable, and neither
  does the operator's own thread count: the same fixture takes ~1.6 s per
  revision at 1, 4 and 8 threads.
- The plain `check` reads no chunk data at all — it only compares ids — so its
  cost is the snapshot files plus the chunk-tree walk.
- The snapshot cache is written but never read for a local storage
  (`IsCacheNeeded()` is false), so the snapshot files are re-read from the
  storage every run; that is the same finding as `snapshot_perf.md`, and it is
  what makes the per-revision read the thing `-threads` should overlap.

The `-files` cost has one removable piece: `VerifySnapshot` walks the file
sequence twice per revision — once in `CheckSnapshot` and again to collect the
files it hashes. `list -files` removed exactly this double pass
(`snapshot_perf.md` fix #5, `38a36fc`); on a local storage the second pass is
served from the chunk cache, so the saving is a re-decode per revision rather
than a round trip. Recorded as candidate #3.

## The call path

`checkSnapshots` (`duplicacy/duplicacy_main.go:960`) parses the flags, creates
the storage with `-threads` (`:982`) and calls
`BackupManager.SnapshotManager.CheckSnapshots` (`:1017`).

`CheckSnapshots` (`src/duplicacy_snapshotmanager.go:962`) then does, in order:

```go
manager.CreateChunkOperator(resurrect, rewriteChunks, threads, allowFailures)   // :965
...
allChunks, allSizes := manager.ListAllFiles(manager.storage, chunkDir)          // :986
...
for snapshotID = range snapshotMap {                                            // :1023
    revisions, err = manager.ListSnapshotRevisions(snapshotID)                  // :1028
    ... manager.downloadSnapshots(snapshotID, revisions, listed, threads)       // :1039  parallel
}
...
if checkFiles {                                                                 // :1069
    manager.DownloadSnapshotSequences(snapshot)                                 // :1070
    manager.VerifySnapshot(snapshot)                                            // :1071
    continue
}
manager.GetSnapshotChunkHashes(snapshot, allChunkHashes, chunks)                // :1077
... manager.storage.FindChunk(0, chunkID, false)                                // :1089  existence check
...
if !checkChunks || checkFiles { return true }                                   // :1177
... for chunkHash := range *allChunkHashes { ... skipped if verified }          // :1230
... manager.chunkOperator.Download(chunkHashes[chunkIndex], chunkIndex, false)  // :1265  parallel
```

The phase boundaries are visible in the log: `SNAPSHOT_CHECK` "Listing all
chunks", then "N snapshots and M revisions", then the per-revision
`All chunks referenced by ...` (or `-files`' `SNAPSHOT_VERIFY`) lines.

Two modes are worth separating:

- **`-chunks`** (`checkChunks && !checkFiles`, `:1060` and `:1177`) builds
  `allChunkHashes`, then downloads and hashes every referenced chunk through the
  operator. This is the only mode that verifies chunk *content*, and it is the
  only mode that reads `verified_chunks` (`:1189`) and skips already-verified
  chunks.
- **`-files`** verifies each file by re-reading every chunk the file spans and
  re-computing the file hash (`VerifySnapshot` → `CheckSnapshot` →
  `ListRemoteFiles` → `RetrieveFile`).

The plain `check` (neither flag) does none of that: it only confirms that every
chunk id in `snapshotMap` exists in `chunkSizeMap`, with a `FindChunk` fallback
for the first 100 that look missing (`:1088`).

## The revision loop used to be serial and ignore `-threads` — fixed

`CheckSnapshots` accepts `threads` and passes it to `CreateChunkOperator`
(`:965`), but the read of the snapshot files used to be a plain loop over
`downloadSnapshot` with a hard-coded thread index `0` and a single shared
`manager.fileChunk`. The operator's threads do not help, because the snapshot
file is not a metadata chunk — it goes through `SnapshotManager.downloadFile`,
not through the chunk operator.

`list` (`:883`) and `prune` both read their revisions with `downloadSnapshots`
(`:809`), which fans the same `downloadSnapshot` calls out over `threads`
workers, gives each worker its own `Chunk`, and passes each worker its own thread
index. That helper exists precisely for this loop shape and was not called from
`check`; the loop now calls it:

```go
for _, snapshot := range manager.downloadSnapshots(snapshotID, revisions, listed, threads) {   // :1039
    if tag != "" && snapshot.Tag != tag {
        continue
    }
    snapshotMap[snapshotID] = append(snapshotMap[snapshotID], snapshot)
}
```

The revisions come from `ListSnapshotRevisions`, so `listed` is true as before
and the per-revision existence check is still skipped; the snapshots come back in
revision order, so the tag filter and the appends see the same sequence the
serial loop produced, and everything after the download is unchanged. The
storage is already created with `-threads`
(`duplicacy/duplicacy_main.go:982`), so the workers' thread indexes stay within
what the storage expects.

Measured A/B on a virtiofs mount with 900 revisions of one id whose chunk lists
are shared (so the run is almost all revision reads), `/usr/bin/time`, best of
five, with the cache cleared before every run:

| Build | `-threads 1` | `-threads 4` | `-threads 8` |
| --- | --- | --- | --- |
| HEAD (`downloadSnapshot` loop) | 4.65 s | 4.70 s | 4.63 s |
| with the fix | 4.72 s | 3.96 s | 3.92 s |

The flag is flat on HEAD and worth about 1.2x at eight threads once the loop is
parallel. `-threads 1` is unchanged, because `downloadSnapshots` falls back to
the same `downloadSnapshot` call for a single worker — so the default path and
its ordering are untouched. The output is byte-identical to HEAD at `-threads
1/2/4/8` for `check`, `-chunks`, `-files`, `-stats`, `-tabular` and `-r`/`-t`
restrictions, and the error paths (`check -r 99`, an unknown id) exit and print
the same way.

The same overlap applies to the `-stats`/`-tabular` modes, which force the
revision listing (`:1027`) and are otherwise identical reads.

On a local filesystem the loop is two syscalls per revision, so there is nothing
to overlap, and the helper's per-worker `Chunk` — grown to `MaximumChunkSize`,
16 MB for the default 4M chunk size — makes a high `-threads` slower and much
more memory-hungry than the serial loop. This is the trade-off `list` and `prune`
already accept, not something `check` introduces: on a 300-revision ext4 fixture
`check -threads 32` goes from 0.11 s at 92 MB on HEAD to 0.35 s at 597 MB, while
with a 64K chunk size, where the buffers are small, the two builds are
indistinguishable. A local run that does not name a thread count takes the
single-worker path and is unaffected.

A second, smaller part of phase 1 is that `ListSnapshotRevisions` (`:1028`) is
called once per id on the calling goroutine. Ids are few, so this matters less
than the per-revision reads; overlapping it would need care not to change the
order in which ids appear in `snapshotMap`.

## The chunk-tree walk is paid in full — candidate #2

`CheckSnapshots` lists the whole `chunks/` subtree on every run, before it knows
which revisions were named:

```go
allChunks, allSizes := manager.ListAllFiles(manager.storage, chunkDir)   // :986
```

`ListAllFiles` (`:703`) is a serial BFS that lists the top directory and every
subdirectory. Its cost is proportional to the number of chunk directories, not to
the work the user asked for, and the result is only used to answer "does this
chunk exist" for the chunks the checked revisions reference (`:1082`).

On the 500-revision drvfs fixture the setup cost is visible: a `check -r 1` takes
0.16 s against ~0.01 s of process startup, and the log shows 71 `Listing
chunks/...` lines for a tree of 84 chunks, plus the listing of the 500 snapshot
files of the one id. On a mount where every listing is a round trip the chunk
walk is the same shape as `prune`'s `-exhaustive` tree walk, which
`prune_perf.md` records as the second-order cost after the sequence expansion.

Two reasons it is only a candidate:

- The walk also produces the "has a size of 0" check (`:1000`), which is part of
  what `check` is for, so it cannot be dropped outright.
- Replacing it with a `FindChunk` per referenced chunk would turn a fixed number
  of listing round trips into one round trip per referenced chunk, which is worse
  when the repository is large. The walk wins whenever `chunks/` is smaller than
  the referenced set, which is the usual case.

## `-chunks` already overlaps; `-files` cannot

The two content-verifying modes behave very differently under `-threads`.

**`-chunks` scales.** The verification loop submits each chunk to the operator
(`:1265`), and the operator was created with the user's `-threads` (`:965`).
Measured best of three on a drvfs storage:

| Fixture | `-threads 1` | `-threads 4` | `-threads 8` |
| --- | --- | --- | --- |
| 100 revisions, 366 chunks, 7.9 MB | 1.52 s | 1.17 s | 1.15 s |
| 20 revisions, 137 chunks, 200 MB | 2.05 s | 0.25 s | 0.22 s |

On the second fixture the same command at one thread spends 1.44 s on a single
revision's referenced chunks against 0.55 s at eight, and the process CPU rises
from 47% to 161%, which is the signature of overlap rather than of a smaller
working set.

**`-files` does not.** `VerifySnapshot` (`:1507`) walks the file sequence through
`ListRemoteFiles`, then verifies each file by calling `RetrieveFile` (`:1552`),
which blocks on `manager.chunkOperator.Download` on the calling goroutine
(`:1580`, `:1589`). The operator's threads are used for the file *sequence*
metadata reads but not for the file *content* reads, so the dominant cost is
serial:

| Fixture | command | `-threads 1` | `-threads 4` | `-threads 8` |
| --- | --- | --- | --- | --- |
| 100 revisions, 366 chunks | `check -files -r 1-5` | 0.44 s | 0.47 s | 0.45 s |
| 20 revisions, 137 chunks, 200 MB | `check -files -r 1` | 1.61 s | 1.64 s | 1.59 s |

This is not accidental: `-files` is I/O-bound on reading the file chunks, and on
drvfs the run is about half user time and half system time with ~12,000 voluntary
context switches for one revision. `check -files` on the 20-revision, 200 MB
fixture takes 1.54 s for one revision, 8.9 s for five and 60.9 s for twenty, so
it is linear in the amount of data and dominated by the storage read, not by
anything the command does per revision.

### `check -files` walks the file sequence twice

`VerifySnapshot` (`:1507`) runs two full traversals of the file sequence per
revision. First it calls `CheckSnapshot` (`:2855`) to sanity-check the entries,
and `CheckSnapshot` walks the sequence through `ListRemoteFiles` (`:2866`).
Then `VerifySnapshot` itself walks the same sequence again (`:1518`) to collect
the files it is about to hash:

```go
func (manager *SnapshotManager) VerifySnapshot(snapshot *Snapshot) bool {
    err := manager.CheckSnapshot(snapshot)          // :1507  ListRemoteFiles #1
    ...
    snapshot.ListRemoteFiles(manager.config, manager.chunkOperator, func(file *Entry) bool {   // :1518  #2
        ...
    })
```

`ListRemoteFiles` re-reads and re-decodes every entry, and it fetches every
chunk of the file sequence through the operator as it goes (`:119`). This is the
same double pass `list -files` removed (`snapshot_perf.md` fix #5, `38a36fc`);
`check -files` still pays it.

Measured on a 3000-file repository (one 1-chunk file sequence) with
`-d check -files` and `-d list -files`, cold cache, counting `CHUNK_DOWNLOAD`
against `CHUNK_CACHE`:

```
              check -files            list -files
              downloads  hits         downloads  hits
-r 1              4       1              2       0
-r 1-3            6       9              2       4
-r 1-5            9      16              3       7
```

`check -files` fetches all three metadata sequences per revision and then reads
the file content chunks as it hashes them (`-r 1` is the chunk, length and file
sequences, a cache-served second traversal of the file sequence, and the file's
own content chunk), where `list -files` reads two metadata sequences and no
content. Its cache hits outnumber `list -files`' because the second traversal of
the file sequence is served from the cache. The exact counts depend on how many
revisions share a sequence (`-r 1-5` adds fewer new downloads than `-r 1-3`
did), so only the shape matters.

Unlike `list -files`, `check -files` genuinely needs all three sequences
(`CheckSnapshot` bounds each entry against `snapshot.ChunkLengths` and
`len(snapshot.ChunkHashes)`, and the file hash is computed from
`ChunkHashes`), so the `list -files` fix #8 — dropping the chunk sequence —
does not apply. The candidate here is narrower: make `CheckSnapshot` and
`VerifySnapshot` share one traversal, so the sequence is decoded once per
revision instead of twice. Recorded as candidate #3.

## The snapshot cache is written but never read

Every snapshot file download goes through `downloadFile` (`:2938`), which guards
the cache read with `IsCacheNeeded()` (`:2940`) and the write-back with the same
predicate (`:2983`). For a local path `CreateStorage` leaves `isCacheNeeded`
false, so both are skipped and every run re-reads each snapshot file from the
storage.

That is the same behaviour `snapshot_perf.md` documented for `list`, and it is
why an unparallelised `check` is the slowest way to read a repository: 500
snapshot files are 500 `openat`/`read` pairs on one goroutine. It is not a
`check`-specific defect, so it is not a candidate here; the revision-loop fix is
what lets `-threads` overlap those reads.

## Smaller items

- **The `verified_chunks` skip needs `-chunks`.** The list is read at `:1189` and
  consulted at `:1230`, but `allChunkHashes` is only non-empty for
  `checkChunks && !checkFiles` (`:1060`), so a plain `check` reads the file and
  then has nothing to skip. Harmless, but it means two `check -chunks` runs on an
  unchanged repository do less work the second time while two plain `check` runs
  do not.
- **The existence-check fallback is capped at 100 per revision.** When a
  referenced chunk is missing from `chunkSizeMap`, `check` retries it with
  `FindChunk` but only while `missingChunks < 100` (`:1088`) — a deliberate guard
  against a repository full of missing chunks, and it counts only the missing
  ones, so it is not itself a cost.
- **`-threads` also sets the storage's thread count.** `checkSnapshots` creates
  the storage with `-threads` (`duplicacy/duplicacy_main.go:982`), so the worker
  indexes the loop passes are already in range, exactly as `prune` relies on
  (`prune_perf.md`, "The revision loop used to be serial").
- **`ShowStatistics`/`ShowStatisticsTabular` re-expands sequences.** After the
  existence phase, both (`:1171`, `:1173`) call `GetSnapshotChunks` per revision
  (`:1325`, `:1378`, `:1391`) and thus re-derive the metadata chunk lists a third
  time. They are pure local computation over `snapshot.ChunkSequence` etc. and do
  not touch the storage, so the cost is a slice build per revision, not a round
  trip. It matters only because `-stats` already implies all revisions.

### Deliberately not pursued

- **Parallelising the existence check or the tree walk.** The tree walk would
  have to change the order chunks are listed, and it is bounded by the chunk
  directory count rather than by the data; the existence check is a hash-map
  lookup for every chunk that the listing already found.
- **Verifying file chunks concurrently in `-files`.** It is the obvious next
  lever for the expensive mode, but the file verification is a per-file hash
  computation whose chunks overlap between files, so the overlap would have to be
  at chunk granularity with a shared hash state per file. The win is bounded by
  the storage read, which on drvfs is already using a visible fraction of the
  wall time (48% CPU), and the change would have to preserve the
  `SkipFileHash`/`alternateHash` handling. No measurement was taken; this is a
  guess and is recorded as such.
- **A chunk index.** As in `snapshot_perf.md` (candidate #7), `copy_perf.md` and
  `prune_perf.md`, this would change the storage format on disk.
- **Dropping the whole-tree `chunks/` listing in favour of `FindChunk` per
  referenced chunk.** See candidate #2: it trades a fixed walk for one round trip
  per referenced chunk and loses on large repositories.

## How to confirm on a given setup

- `duplicacy -d check ...` sets DEBUG logging
  (`duplicacy/duplicacy_main.go:142-148`); `-v` sets TRACE. The
  `DOWNLOAD_FILE` lines ("Downloaded file snapshots/<id>/<n>") show the revision
  loop: with `-threads 1` they are printed one after another, and with more
  threads they interleave. Compare with `duplicacy -d list` on the same
  repository, which already overlaps them.
- `duplicacy -d check` prints `LIST_FILES` ("Listing chunks/...") once per chunk
  directory: that count is the whole-tree walk, and it does not change when the
  command is restricted with `-r`.
- The `CHUNK_DOWNLOAD` ("Chunk ... has been downloaded") and `CHUNK_CACHE`
  ("loaded from the snapshot cache") counts separate the modes: a plain `check`
  fetches only the metadata chunks of the referenced sequences, `-chunks` fetches
  every referenced chunk, and `-files` fetches every chunk of every verified
  file. The `CHUNK_CACHE` lines are what the unguarded write-back leaves behind
  locally even though the storage asks for no cache.
- `/usr/bin/time -v duplicacy check ...`: the plain mode is syscall-bound (low
  CPU, high context-switch count), `-chunks` becomes CPU-bound once `-threads` is
  raised (CPU above 100%), and `-files` stays around half the wall time in
  system time because it is bound by the storage read.
- Sum the per-call wall time of `newfstatat`, `openat`, `read` and `mkdirat` with
  the `strace` recipe in `docs/README.md`. On the 100-revision drvfs fixture a
  plain `check` issues 1207 `newfstatat`, 504 `openat` and 420 `read` calls and
  `-chunks` roughly doubles the `openat`/`read` counts; `-files` is dominated by
  `read`.
- `check -r 5` against a full `check` gives the per-revision marginal cost
  against the fixed setup cost — the gap is the chunk-tree walk plus the revision
  listing, which is what candidate #2 is about.
- `check -r 1-20 -threads 1` against `-threads 8` on a storage where a read is a
  round trip isolates the fix: on a native filesystem the per-revision read is a
  couple of syscalls and the difference is small, while on virtiofs the flag is
  flat on HEAD and worth about 1.2x once the loop is parallel. Note that a very
  high thread count on a local filesystem costs memory and time instead, because
  each worker holds a `MaximumChunkSize` buffer (see the finding above).
- `go test ./src/ -vet=off` is the safety net for any change to this path;
  `TestCheckDownloadsRevisionsConcurrently`,
  `TestPruneDownloadsRevisionsConcurrently` and
  `TestDownloadSnapshotsConcurrently`
  (`src/duplicacy_snapshotmanager_test.go:593`, `:541`, `:460`) pin the overlap
  down and fail with "at most 1 was in flight at a time" if the serial loop is
  put back.
