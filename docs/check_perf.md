# Where `check` spends its time

Investigation into the performance of `duplicacy check`, in the style of
`snapshot_perf.md`, `copy_perf.md`, `prune_perf.md` and `init_perf.md`. Three
defects were found and fixed. The revision loop ignored `-threads`, exactly as
prune's did before `c651bb1`; it now reads its snapshot files through
`downloadSnapshots`, the same helper `list` and `prune` use. The whole chunk tree
was listed on every run, even when a `-r` restriction had named a handful of
revisions; the listing is now weighed against the referenced set, and a
restricted check that references fewer chunks than the tree holds looks them up
instead. And `-files` walked the file sequence twice per revision, checking it
and then collecting from it; the check and the collection are now one walk. The
`check -files` mode is still the expensive one, but its dominant cost — reading
the file chunks — is inherent to what the mode promises rather than to a
mistake.

## Summary

`check` is three phases:

1. a read of the `snapshots/` directory per id and one snapshot file per
   revision, to build `snapshotMap`;
2. a walk of the whole `chunks/` subtree, to build `chunkSizeMap` — or, when the
   checked revisions reference fewer chunks than the tree holds, a lookup of just
   those chunks;
3. per revision, either a referenced-chunk existence check, or the file
   verification of `-files`.

The first phase is where the first defect was, and it is fixed. `CheckSnapshots`
(`src/duplicacy_snapshotmanager.go:1033`) read the snapshot files one at a time on
the calling goroutine although it already accepted `-threads` and spends it on
the chunk operator (`:1036`). `list` and `prune` both read their revisions through
`downloadSnapshots` (`:880`) under `-threads`; `check` was simply never switched
over, so the flag was inert for the largest per-item cost of the command's first
phase. It now calls the helper (`:1089`). Measured on a virtiofs mount whose run
is almost all revision reads, the flag is flat on HEAD and worth about 1.2x once
the loop is parallel, with `-threads 1` unchanged.

The second phase is a `ListAllFiles` of the entire chunk tree (`:1114`), which is
proportional to the number of chunk directories rather than to the revisions
being checked. It used to be paid in full even by `check -r 5`, and its result
(`chunkSizeMap`) is only ever consulted for the chunks the named revisions
reference; it is now decided by weighing one count of the chunk directories
against the size of the referenced set, and a restricted check that references
fewer chunks than the tree holds looks them up (`probeChunks`, `:763`) instead.
On a slow mount `check -r 2` over a 1967-chunk tree goes from 0.90 s to 0.10 s.

The third phase is proportional to the data. The plain `check` only compares the
referenced chunk ids against the listing, so it does not read a chunk at all;
`-chunks` downloads every referenced chunk to verify its hash and does use
`-threads`; `-files` reads every file chunk, on one goroutine, and is the mode
that costs seconds to minutes.

## Conclusion

**All three defects were worth fixing and all three are fixed.** The revision
loop reads its snapshot files through `downloadSnapshots`, so `-threads` reaches
the per-revision read — the change already applied to `prune` in `c651bb1` and to
`list` in `6584fb0`: a few lines, byte-identical to the serial loop at one thread
and to HEAD's output at every thread count. The chunk-tree listing is no longer
unconditional: a restricted check that references fewer chunks than the tree
holds answers the existence question with one lookup per referenced chunk instead
of one listing per chunk directory, the same rule `copy` applies to the identical
listing. And `-files` checks and collects the file list in one walk instead of
two, which is the fix `list -files` already had.

The first two are byte-identical to HEAD apart from the two log lines that
describe the phase that was skipped, and the second leaves the unrestricted check
on the walk; `-files` is output-identical outright. The figures are below.

Nothing else is a defect:

- `-chunks` already overlaps its verification, because `CheckSnapshots` creates
  the chunk operator with the user's `-threads` (`:1036`) and the verification
  loop submits through it (`:1362`). On a 20-revision, 200 MB fixture `-chunks`
  goes from 2.05 s at one thread to 0.22 s at eight.
- `-files` is I/O-bound on the file chunks themselves, and `VerifySnapshot`
  (`:1604`) hands those to `RetrieveFile` (`:1735`), which blocks on the calling
  goroutine. Raising `-threads` therefore changes nothing measurable, and neither
  does the operator's own thread count: the same fixture takes ~1.6 s per
  revision at 1, 4 and 8 threads.
- The plain `check` reads no chunk data at all — it only compares ids — so its
  cost is the snapshot files plus the chunk-tree walk.
- The snapshot cache is written but never read for a local storage
  (`IsCacheNeeded()` is false), so the snapshot files are re-read from the
  storage every run; that is the same finding as `snapshot_perf.md`, and it is
  what makes the per-revision read the thing `-threads` should overlap.

The `-files` cost had one removable piece: `VerifySnapshot` walked the file
sequence twice per revision — once in `CheckSnapshot` and again to collect the
files it hashes. `list -files` removed exactly this double pass
(`snapshot_perf.md` fix #5, `38a36fc`), and `check -files` now shares one walk
too; the second pass was served from the chunk cache on a storage that has one,
so the saving was a re-decode per revision plus a cache read.

## The call path

`checkSnapshots` (`duplicacy/duplicacy_main.go:960`) parses the flags, creates
the storage with `-threads` (`:982`) and calls
`BackupManager.SnapshotManager.CheckSnapshots` (`:1017`).

`CheckSnapshots` (`src/duplicacy_snapshotmanager.go:1033`) then does, in order:

```go
manager.CreateChunkOperator(resurrect, rewriteChunks, threads, allowFailures)   // :1036
...
for snapshotID = range snapshotMap {                                            // :1073
    revisions, err = manager.ListSnapshotRevisions(snapshotID)                  // :1078
    ... manager.downloadSnapshots(snapshotID, revisions, listed, threads)       // :1089  parallel
}
...
listChunks = manager.countChunkDirectories() <= len(referencedChunks)           // :1109  the decision
if listChunks {
    allChunks, allSizes := manager.ListAllFiles(manager.storage, chunkDir)      // :1114  the walk
} else {
    emptyChunks = manager.probeChunks(...)                                      // :1137  or the lookups
}
...
if checkFiles {                                                                 // :1165
    manager.DownloadSnapshotSequences(snapshot)                                 // :1166
    manager.VerifySnapshot(snapshot)                                            // :1167
    continue
}
manager.GetSnapshotChunkHashes(snapshot, allChunkHashes, chunks)                // :1173
... manager.storage.FindChunk(0, chunkID, false)                                // :1186  existence check
...
if !checkChunks || checkFiles { return true }                                   // :1274
... for chunkHash := range *allChunkHashes { ... skipped if verified }          // :1327
... manager.chunkOperator.Download(chunkHashes[chunkIndex], chunkIndex, false)  // :1362  parallel
```

The phase boundaries are visible in the log: `SNAPSHOT_CHECK` "Listing all
chunks" (or "Checking the N chunks referenced by the listed revisions"), then "N
snapshots and M revisions", then the per-revision `All chunks referenced by ...`
(or `-files`' `SNAPSHOT_VERIFY`) lines.

Two modes are worth separating:

- **`-chunks`** (`checkChunks && !checkFiles`, `:1156` and `:1274`) builds
  `allChunkHashes`, then downloads and hashes every referenced chunk through the
  operator. This is the only mode that verifies chunk *content*, and it is the
  only mode that reads `verified_chunks` (`:1286`) and skips already-verified
  chunks.
- **`-files`** verifies each file by re-reading every chunk the file spans and
  re-computing the file hash (`VerifySnapshot` → `CheckSnapshot` →
  `ListRemoteFiles` → `RetrieveFile`).

The plain `check` (neither flag) does none of that: it only confirms that every
chunk id in `snapshotMap` exists in `chunkSizeMap`, with a `FindChunk` fallback
for the first 100 that look missing (`:1186`).

## The revision loop used to be serial and ignore `-threads` — fixed

`CheckSnapshots` accepts `threads` and passes it to `CreateChunkOperator`
(`:1036`), but the read of the snapshot files used to be a plain loop over
`downloadSnapshot` with a hard-coded thread index `0` and a single shared
`manager.fileChunk`. The operator's threads do not help, because the snapshot
file is not a metadata chunk — it goes through `SnapshotManager.downloadFile`,
not through the chunk operator.

`list` (`:915`) and `prune` both read their revisions with `downloadSnapshots`
(`:880`), which fans the same `downloadSnapshot` calls out over `threads`
workers, gives each worker its own `Chunk`, and passes each worker its own thread
index. That helper exists precisely for this loop shape and was not called from
`check`; the loop now calls it:

```go
for _, snapshot := range manager.downloadSnapshots(snapshotID, revisions, listed, threads) {   // :1089
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

A second, smaller part of phase 1 is that `ListSnapshotRevisions` (`:1078`) is
called once per id on the calling goroutine. Ids are few, so this matters less
than the per-revision reads; overlapping it would need care not to change the
order in which ids appear in `snapshotMap`.

## The chunk-tree walk is paid in full — fixed

`CheckSnapshots` used to list the whole `chunks/` subtree on every run, before it
knew which revisions were named:

```go
allChunks, allSizes := manager.ListAllFiles(manager.storage, chunkDir)   // :1114, listed branch
```

`ListAllFiles` (`:703`) is a serial BFS that lists the top directory and every
subdirectory. Its cost is proportional to the number of chunk directories, not to
the work the user asked for, and the result is only used to answer "does this
chunk exist" for the chunks the checked revisions reference (`:1178`).

The cost is real and it is paid where it buys the least. On a virtiofs fixture of
1967 chunks in 256 directories, where a revision references 43 of them, a
`check -r 2` took 0.90 s — of which the syscall time is 0.14 s and the rest is
the mount's per-listing latency — against 0.01 s of process startup, and
`list -r 2` on the same repository takes 0.02 s. The `-v` log shows 257 `Listing
chunks/...` lines: one call per directory plus the tree root. The whole-tree walk
is the same shape as `prune`'s `-exhaustive` tree walk, which `prune_perf.md`
records as the second-order cost after the sequence expansion.

What the walk buys is a map from chunk id to size for the whole tree, consulted
in two places: the existence check (`:1178`) and the "has a size of 0" report
(`:1128`, `:1258`). Two things keep the obvious replacement from being a pure
win, and both are why the fix is a decision rather than a deletion:

- The walk answers the existence question for every chunk at the cost of one
  listing per chunk directory, while a lookup per referenced chunk costs one
  `FindChunk` per chunk. So the walk wins whenever the tree is smaller than the
  referenced set — which includes the unrestricted check, where the referenced
  set *is* the tree. Probing every chunk there regresses a full check on the same
  fixture from 0.87 s to 1.51 s.
- The size-of-0 check is part of what `check` is for, so it cannot be dropped;
  the probe has to record the sizes it sees.

The fix weighs the two, exactly as `copy` weighs the identical listing against
the chunks it is about to copy (`copy_perf.md`, "The destination chunk listing is
unconditional"): `countChunkDirectories()` (`:743`) makes one `ListFiles(0,
"chunks/")` call and counts the directories, and the walk runs only when that
count is no greater than the size of the referenced set (`:1109`). The referenced
set comes from `referencedChunkIDs` (`:799`), which expands the chunk sequences;
it is only computed for the existence-only check, because `-chunks` downloads
every referenced chunk and `-files` reads every referenced chunk, so both would
pay for a lookup on top of the read that proves the chunk anyway.

When the walk is skipped, `probeChunks` (`:763`) looks each referenced chunk up
and fills `chunkSizeMap` with the sizes and the size-of-0 count the walk would
have produced. The lookups are independent and go through `runConcurrently`, so
`-threads` reaches them, and the map itself is written by the calling goroutine.

Measured on the 1967-chunk/256-directory virtiofs fixture, `/usr/bin/time`, best
of five, cache cleared before every run:

| Command | HEAD | with the fix |
| --- | --- | --- |
| `check -r 2` (43 referenced chunks) | 0.90 s | 0.10 s |
| `check -r 2 -threads 8` | 0.87 s | 0.10 s |
| `check -r 1` (1969 referenced chunks) | 0.91 s | 0.95 s |
| `check` (unrestricted) | 0.91 s | 0.89 s |

The restricted check no longer pays for the tree it does not read; the
unrestricted check is unchanged, because its referenced set is the whole tree and
the comparison keeps the walk. `-chunks -r 2` is also unchanged, for the reason
above.

The output differs in exactly two lines, both of which describe the phase that
was skipped: `Listing all chunks` is replaced by `Checking the N chunks
referenced by the listed revisions`, and `Total chunk size is X in M chunks`
then reports `M == N` — the size of the checked chunks rather than of the whole
tree, which is the same number the run has always printed *for a fully listed
repository*. Everything else is byte-identical, including the missing-chunk
detection (`check -r 2` after deleting a referenced chunk, exit 100), the
`-fossils`/`-resurrect` paths, `-stats`, `-tabular`, `-t`, and the error paths
(`check -r 99`, an unknown id).

Reading the revisions first moves the failure of a nonexistent `-r` revision
ahead of the listing, so `check -r 999` now prints its error without the
`Listing all chunks` line that used to precede it. The error itself and the exit
code are unchanged; what changes is that a run that is certain to fail no longer
pays for the tree walk first.

Two details make the probe path safe to take the lookups:

- The `FindChunk` fallback for a chunk that looks missing (`:1186`) is skipped
  when the tree was not listed. It exists to confirm a chunk that the listing's
  walk did not reach and is capped at 100 per revision; when the walk has not run
  the same lookup has already been asked by `probeChunks`, so repeating it would
  only double the round trips.
- A chunk the probe finds with a size of 0 is reported and fails the run, the
  same as the listing would have made it fail.

Covered by `TestCheckListsTheChunkTreeOnlyWhenItIsCheaper`, which fails with "the
restricted check listed N chunk directories" if the decision is forced back to
the walk, and `TestCheckProbesReportTheChunkSizes`, which truncates a probed
chunk and checks the run fails on it.

## `-chunks` already overlaps; `-files` cannot

The two content-verifying modes behave very differently under `-threads`.

**`-chunks` scales.** The verification loop submits each chunk to the operator
(`:1362`), and the operator was created with the user's `-threads` (`:1036`).
Measured best of three on a drvfs storage:

| Fixture | `-threads 1` | `-threads 4` | `-threads 8` |
| --- | --- | --- | --- |
| 100 revisions, 366 chunks, 7.9 MB | 1.52 s | 1.17 s | 1.15 s |
| 20 revisions, 137 chunks, 200 MB | 2.05 s | 0.25 s | 0.22 s |

On the second fixture the same command at one thread spends 1.44 s on a single
revision's referenced chunks against 0.55 s at eight, and the process CPU rises
from 47% to 161%, which is the signature of overlap rather than of a smaller
working set.

**`-files` does not.** `VerifySnapshot` (`:1604`) walks the file sequence through
`ListRemoteFiles`, then verifies each file by calling `RetrieveFile` (`:1649`),
which blocks on `manager.chunkOperator.Download` on the calling goroutine
(`:1677`, `:1686`). The operator's threads are used for the file *sequence*
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

### `check -files` used to walk the file sequence twice — fixed

`VerifySnapshot` (`:1604`) ran two full traversals of the file sequence per
revision. First it called `CheckSnapshot` to sanity-check the entries, and that
walked the sequence through `ListRemoteFiles`. Then `VerifySnapshot` itself
walked the same sequence again to collect the files it was about to hash:

```go
func (manager *SnapshotManager) VerifySnapshot(snapshot *Snapshot) bool {
    err := manager.CheckSnapshot(snapshot)          // ListRemoteFiles #1
    ...
    snapshot.ListRemoteFiles(manager.config, manager.chunkOperator, func(file *Entry) bool {   // #2
        ...
    })
```

`ListRemoteFiles` re-reads and re-decodes every entry, and it fetches every
chunk of the file sequence through the operator as it goes (`:119`). That makes
the second traversal more than a re-decode: the first traversal fetches the
sequence from the storage and the operator writes each metadata chunk to the
snapshot cache, so the second traversal is served from the cache instead. On a
storage that needs no cache the sequence is not written to it at all, so there
the second traversal reads the storage again.

This is the same double pass `list -files` removed (`snapshot_perf.md` fix #5,
`38a36fc`); `check -files` still paid it. The fix is the one that document
describes: `CheckSnapshot` and the file-collecting traversal are now one walk,
`walkSnapshotEntries` (`:1645`), which checks each entry as it visits it and
collects the files of a non-zero size on the same pass. The per-entry checks are
unchanged — they are the body of `checkRemoteEntry` (`:1681`), called from the
collector — so an invalid entry still stops the walk and its error is reported
before any file is hashed, and `CheckSnapshot` (`:3039`) remains as the
checking-only form for callers outside the package.

Measured on a repository of 40,000 files whose file sequence spans 415 metadata
chunks, cold cache:

| Command | HEAD | with the fix |
| --- | --- | --- |
| `check -files -r 1`, repository and cache on a slow mount | 8.23 s | 7.03 s |
| `check -files -r 1`, repository and cache on ext4 | 1.52 s | 1.47 s |
| chunks fetched | 419 + 415 cache reads | 419 |

The `-d` counts are the cleanest confirmation: the 40,000 files are 415 metadata
chunks of file sequence plus the two short sequences and the two file-content
chunks, so HEAD fetches all 419 and then reads 415 of them from the cache a
second time, while the fix fetches the same 419 and makes no cache read. The win
is the largest where a cache read is a round trip — the cache lives next to the
repository, so a repository on a network mount is exactly that case — and it is
small on a native filesystem, where both the re-decode and the cache read are
cheap.

The output is unchanged: `check`, `check -files`, `check -stats`, `check -stats
-files`, `check -tabular -files`, `check -chunks`, `list -files` and the corrupt
-file path (`check -files` after flipping bytes in a file chunk, exit 100) are
byte-identical to HEAD.

One latent defect in the checks is fixed along the way: the branch for an entry
whose end chunk precedes its start chunk called `fmt.Errorf` without assigning
the result, so HEAD stopped the walk but reported nothing, and the file was then
hashed anyway. The single walk returns that error, so a snapshot malformed in
that particular way is now reported as an error instead of as a corrupted file.
The branch is only reachable from a snapshot whose delta-encoded chunk indexes
decode to an inverted range.

Covered by `TestCheckFilesWalksTheFileSequenceOnce`, which fails with "the file
sequence was walked twice: 1 chunks were read from the cache again" when the
second walk is put back.

## The snapshot cache is written but never read

Every snapshot file download goes through `downloadFile` (`:3035`), which guards
the cache read with `IsCacheNeeded()` (`:3037`) and the write-back with the same
predicate (`:3080`). For a local path `CreateStorage` leaves `isCacheNeeded`
false, so both are skipped and every run re-reads each snapshot file from the
storage.

That is the same behaviour `snapshot_perf.md` documented for `list`, and it is
why an unparallelised `check` is the slowest way to read a repository: 500
snapshot files are 500 `openat`/`read` pairs on one goroutine. It is not a
`check`-specific defect, so it is not a candidate here; the revision-loop fix is
what lets `-threads` overlap those reads.

## Smaller items

- **The `verified_chunks` skip needs `-chunks`.** The list is read at `:1286` and
  consulted at `:1327`, but `allChunkHashes` is only non-empty for
  `checkChunks && !checkFiles` (`:1156`), so a plain `check` reads the file and
  then has nothing to skip. Harmless, but it means two `check -chunks` runs on an
  unchanged repository do less work the second time while two plain `check` runs
  do not.
- **The existence-check fallback is capped at 100 per revision.** When a
  referenced chunk is missing from `chunkSizeMap`, `check` retries it with
  `FindChunk` but only while `missingChunks < 100` (`:1186`) — a deliberate guard
  against a repository full of missing chunks, and it counts only the missing
  ones, so it is not itself a cost. The fallback is skipped entirely when the
  whole-tree walk did not run, since the probe has already asked the same
  question.
- **`-threads` also sets the storage's thread count.** `checkSnapshots` creates
  the storage with `-threads` (`duplicacy/duplicacy_main.go:982`), so the worker
  indexes the loop passes are already in range, exactly as `prune` relies on
  (`prune_perf.md`, "The revision loop used to be serial").
- **`ShowStatistics`/`ShowStatisticsTabular` re-expand sequences.** After the
  existence phase, both (`:1268`, `:1270`) call `GetSnapshotChunks` per revision
  (`:1422`, `:1475`, `:1488`) and thus re-derive the metadata chunk lists a third
  time. They are pure local computation over `snapshot.ChunkSequence` etc. and do
  not touch the storage, so the cost is a slice build per revision, not a round
  trip. It matters only because `-stats` already implies all revisions.

### Deliberately not pursued

- **Parallelising the tree walk itself.** What the walk leaves the caller is a
  map keyed by chunk id, so its listings could overlap without changing the
  order chunks are *reported* in, but its cost is one listing per chunk directory
  rather than a round trip per chunk, and the `-r` case that made the walk worth
  attacking is already served by the lookups.
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
- **Dropping the whole-tree `chunks/` listing outright in favour of `FindChunk`
  per referenced chunk.** Measured and rejected: it trades a fixed walk for one
  lookup per referenced chunk and loses whenever the tree is smaller than the
  referenced set, which includes the unrestricted check. This is why the fix is
  the comparison rather than the replacement.

## How to confirm on a given setup

- `duplicacy -d check ...` sets DEBUG logging
  (`duplicacy/duplicacy_main.go:142-148`); `-v` sets TRACE. The
  `DOWNLOAD_FILE` lines ("Downloaded file snapshots/<id>/<n>") show the revision
  loop: with `-threads 1` they are printed one after another, and with more
  threads they interleave. Compare with `duplicacy -d list` on the same
  repository, which already overlaps them.
- `duplicacy -d check` prints `LIST_FILES` ("Listing chunks/...") once per chunk
  directory: that count is the whole-tree walk. On a tree larger than the
  referenced set it is now absent for a restricted check (`-r`), which prints one
  `Checking the N chunks referenced by the listed revisions` line instead; a full
  `check` on the same repository still prints one `LIST_FILES` line per chunk
  directory.
- The `CHUNK_DOWNLOAD` ("Chunk ... has been downloaded") and `CHUNK_CACHE`
  ("loaded from the snapshot cache") counts separate the modes: a plain `check`
  fetches only the metadata chunks of the referenced sequences, `-chunks` fetches
  every referenced chunk, and `-files` fetches every chunk of every verified
  file. The `CHUNK_CACHE` lines are what the unguarded write-back leaves behind
  locally even though the storage asks for no cache.
- `check -files` on a repository whose file sequence spans many metadata chunks
  is the direct test of the single walk: with `-d`, count the `CHUNK_DOWNLOAD`
  and `CHUNK_CACHE` lines. The sequence of a large file list is fetched once and
  read from the cache no times, where a second walk would add one `CHUNK_CACHE`
  line per chunk of the sequence.
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
  against the fixed setup cost — the gap is the revision listing plus whatever
  the chunk-tree listing costs.
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
  put back. `TestCheckListsTheChunkTreeOnlyWhenItIsCheaper` (`:690`) and
  `TestCheckProbesReportTheChunkSizes` (`:801`) pin the walk decision and the
  sizes the probe records, and fail if the walk is forced back or the sizes are
  dropped. `TestCheckFilesWalksTheFileSequenceOnce` (`:737`) pins the single
  file-sequence walk and fails with "the file sequence was walked twice" if the
  second walk is put back.
