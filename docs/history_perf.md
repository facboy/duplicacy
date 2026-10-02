# Where `history` spends its time

Review of `duplicacy history`, in the style of `snapshot_perf.md`, `copy_perf.md`,
`prune_perf.md`, `check_perf.md`, `restore_perf.md`, `backup_perf.md` and
`init_perf.md`. This one is a code review rather than a measured investigation:
the command is small, and every finding is the same shape as a defect that has
already been measured and fixed elsewhere. Candidates #1 (the unread chunk-hash
sequence) and #2 (the serial revision loop) are implemented; candidate #3 (the
existence check on an explicit `-r`) is recorded and not implemented; the fourth
finding is rejected with the chunk index.

## Summary

`history` prints one line per revision of a single file. It reads only metadata —
one snapshot file and the sequences, never a file chunk — so its absolute cost is
small; the cost that matters is one metadata chunk set per revision, and a defect
used to double that.

`ShowHistory` (`src/duplicacy_snapshotmanager.go:2091`) loops over revisions and,
for each one, downloads the snapshot file, expands the length sequence and walks
the file sequence to find the entry. Four properties cost it work:

1. it expands the chunk-hash sequence, which it never reads (candidate #1) — the
   defect `snapshot_perf.md` fix #8 fixed for `list -files`, repeated once per
   revision. **Implemented**;
2. the loop is serial and the operator is created with one thread (candidate #2)
   — the fix #4 shape that `check`, `restore`, `prune` and `backup` all adopted
   after `list`; `history` was the last revision loop without it. **Implemented**;
3. an explicit `-r` pays the per-revision existence check (candidate #3) — the
   fix #3 shape, though here it is weaker because the revision list came from the
   user rather than from a directory listing;
4. `FindFile` walks the file sequence from the start for every revision
   (candidate #4) — inherent to the format, and the chunk-index item already
   rejected in `snapshot_perf.md`, `copy_perf.md` and `prune_perf.md`.

Candidate #1 was worth fixing because the same bytes were read for nothing, the
grounds on which fix #8 was made for `list -files`; `history` scales with the
number of revisions, so the doubled metadata read was paid once per revision.
Candidate #2 was fixed on the same grounds as the parallel `list` and `check`:
the reads did not overlap however many threads were asked for, and the command
now takes `-threads` for the same reason they do. Candidate #3 is recorded and
not implemented, because it is not a pure deletion: skipping the existence check
turns a missing revision into a parse error.

## The call path

`showHistory` (`duplicacy/duplicacy_main.go:1125`) creates the storage with the
`-threads` count (`:1140`) and calls
`BackupManager.SnapshotManager.ShowHistory` (`:1163`), passing that count.

`ShowHistory` (`src/duplicacy_snapshotmanager.go:2091`) then does:

```go
manager.CreateChunkOperator(false, false, threads, false)           // :2100  -threads
defer manager.stopChunkOperator()

if len(revisions) == 0 {
    revisions, err = manager.ListSnapshotRevisions(snapshotID)      // :2107
    listed = true                                                    // :2112
}

snapshots := manager.downloadSnapshots(snapshotID, revisions, listed, threads)  // :2117

var lastVersion *Entry
sort.Ints(revisions)                                                 // :2120
for i, revision := range revisions {                                 // :2121
    snapshot := snapshots[i]
    manager.DownloadSnapshotSequence(snapshot, "lengths")           // :2125  lengths only
    file := manager.FindFile(snapshot, filePath, true)              // :2126
    ... print file.Hash / file.Size ...
}

stat, err := os.Stat(joinPath(top, filePath))                        // :2146  local
... if showLocalHash { manager.config.ComputeFileHash(...) }         // :2154  local
```

`downloadSnapshot` (`:248`) checks the snapshot file exists with `GetFileInfo`
unless `listed` is true (`:253-265`), then reads it with `downloadFile`
(`:267`). `DownloadSnapshotSequence` (`:478`) loads the one sequence it is asked
for, and the plural `DownloadSnapshotSequences` (`:508`) is the pair of
`"chunks"` and `"lengths"` calls; history now uses the singular form.
`FindFile` (`:1802`) walks the file sequence through `ListRemoteFiles`
(`src/duplicacy_snapshot.go:107`) and stops at the matching entry.

## The chunk-hash sequence is expanded and never read — candidate #1

`DownloadSnapshotSequences` loads both sequences (`:508-514`), and `ShowHistory`
uses only what the file sequence carries: `file.Hash`, `file.Size` and
`IsSameAs`. The chunk-hash sequence is read by `RetrieveFile`
(`src/duplicacy_snapshotmanager.go:1735`) through `snapshot.ChunkHashes[i]`, and
`history` never calls it. The file walk needs the length sequence alone, because
`Entry.check` (`src/duplicacy_entry.go:889`) validates each entry against
`snapshot.ChunkLengths`.

This is `snapshot_perf.md` fix #8 exactly: the `showFiles` branch of `list` used
to call `DownloadSnapshotSequences` and read only the length sequence, and
dropping the chunk sequence removed one metadata chunk set per revision — one
round trip on cloud storage, and on a local mount the syscalls plus the cache
write. Measured there, `list -files` cold on a slow mount went from 13.0-16.8 s
to 7.4-7.5 s and the cache entries for `-r 1-100` from 227 to 131
(`docs/snapshot_perf.md:311-325`).

`history` pays it once per revision as well, and without `-r` the loop covers
every revision. **Implemented**: `ShowHistory` now calls
`manager.DownloadSnapshotSequence(snapshot, "lengths")` where it called
`DownloadSnapshotSequences` (`:2118`), so the chunk sequence is no longer
expanded. Unlike fix #8, which only changed the `showFiles` branch, `ShowHistory`
has no other consumer of the chunk sequence, so the single call is the whole
change; `cat` and `diff` of a file still read it and are not touched.

`TestShowHistoryFetchesOnlyTheLengthSequence`
(`src/duplicacy_snapshotmanager_test.go`) pins it: the file and length sequences
are fetched once each and the printed revision line still carries the entry's
hash and size. Removing the change fails it with "Expecting the file and length
sequences to be fetched once each, got 3 fetches".

## The revision loop is serial and the operator is one-threaded — candidate #2 — **Implemented**

`history` had no `-threads` flag, the storage was created with one thread, and the
manager's operator was created with one (`:2097` in the original). `ShowHistory`
called `downloadSnapshot` itself inside its loop rather than the
`downloadSnapshots` helper (`:880`) that `ListSnapshots` uses for the same
revision-list shape.

Each iteration was therefore: the snapshot file download, then the length
sequence chunk set, then the file-sequence walk, all on one thread. Against cloud
storage each metadata chunk is a round trip; on a slow mount each is syscalls.
The parallel-`list` fix (#4) exists for exactly this loop shape, and `check`,
`restore`, `prune` and `backup` have since adopted it — `history` was the last
revision loop without it.

**Implemented**: the command gained a `-threads` flag
(`duplicacy/duplicacy_main.go:1876`), `showHistory` creates the storage with it
(`:1140`) and passes it to `ShowHistory`, whose signature now takes it
(`src/duplicacy_snapshotmanager.go:2095`). The snapshot files are read through
`downloadSnapshots(snapshotID, revisions, listed, threads)` (`:2117`), which
already spreads the workers over the thread indexes the storage was created with,
and the operator is created with the same count (`:2100`) so the length-sequence
expansions overlap too. The expansions and the printing stay in revision order,
so the output is unchanged; a single-threaded call is the plain loop it was
before.

The value only shows where a metadata read is a round trip or a slow syscall, on
a repository with many revisions. On a local ext4 fixture `prune_perf.md`
measured `history` at 0.01-0.03 s and flat between builds because the whole
repository's sequences are a handful of chunks; that is the fixture, not the
shape.

`TestShowHistoryReadsRevisionsConcurrently`
(`src/duplicacy_snapshotmanager_test.go`) pins it: with four threads the snapshot
reads must overlap and stay within the four thread indexes the storage was
created with, and the printed lines must be identical to the single-threaded run.
Reverting the `downloadSnapshots` call to one thread fails it with "With 4
threads: the revisions were not read concurrently, at most 1 was in flight at a
time".

## Explicit `-r` pays the existence check — candidate #3

`ShowHistory` passes `listed` through to `downloadSnapshot`, and `listed` is true
only when `ShowHistory` listed the revisions itself (`:2112`). With `-r`,
`listed` is false, so `downloadSnapshot` runs `GetFileInfo` (`:253-265`) before
`downloadFile` (`:267`): an extra round trip per revision that `list -r` and
`check -r` no longer pay, because their revisions came from a directory listing
(`snapshot_perf.md` fix #3).

This candidate is weaker than it is for `list`, and the difference is worth
recording. For `list`, the revisions came from `ListSnapshotRevisions`, which
already enumerated the directory and knows they exist, so the check is provably
redundant. For `history -r 40`, the user supplied `40` and it may not exist; with
the check skipped, `downloadFile` fails and the error is reported as a parse
failure of the snapshot rather than "Snapshot ... at revision 40 does not
exist". So the fix is not a pure deletion: it either accepts a worse message or
keeps a list-and-check for the explicit case, which is a design change for one
round trip per revision. Recorded as examined.

## `FindFile` walks the file sequence per revision — candidate #4

`FindFile` (`:1802`) streams the file sequence from the start until the path
matches (`return false` stops the walk), so each revision costs a walk
proportional to the entry's position in the sorted list, plus the decode of every
entry before it. This is a property of reading an append-only sequence and is the
same limitation the chunk-index item addresses; it would change the storage
format, so it is rejected here as it is in `snapshot_perf.md` (candidate #7),
`copy_perf.md` and `prune_perf.md`. Recorded only.

## Candidate fixes

### Smaller items

- **The local work is the expected local work.** `os.Stat` (`:2146`) and, under
  `-hash`, `ComputeFileHash` (`:2154`) are the command's local reads. The stat is
  one call per run, not per revision.
- **The chunk-cache `fsync` is already handled.** Each sequence chunk a revision
  expands went through `DownloadSequence` and the snapshot cache, which used to
  `fsync` every cached write. `prune_perf.md` (option 2) removed that for the
  chunk cache, and `check_perf.md` made the cache decision filesystem-based, so
  `history` already benefits without a change of its own.
- **The one-thread `CreateChunkOperator` is load-bearing.** It must exist for
  `FindFile`'s `ListRemoteFiles` to have a non-nil operator, the same reason
  recorded in `backup_perf.md`. Candidate #2 was about the count, not the call.
- **`-hash` hashes the current file, not the revisions.** The per-revision loop
  reads only snapshot metadata, so the flag's cost is one local file hash at the
  end and is what the flag promises.

### Deliberately not pursued

- **A chunk or revision index.** As in `snapshot_perf.md` (candidate #7),
  `copy_perf.md` and `prune_perf.md`, this would change the storage format on
  disk.

## How to confirm on a given setup

- `duplicacy -d history <file>` sets DEBUG logging (`duplicacy/duplicacy_main.go:142-148`);
  `-v` sets TRACE. `SNAPSHOT_LIST_REVISIONS` and `DOWNLOAD_FILE` mark the
  per-revision downloads; timing the gap between consecutive `SNAPSHOT_HISTORY`
  lines gives the per-revision cost, and the `DOWNLOAD_FILE` count per revision
  shows how many sequences it reads.
- Candidate #1: count the metadata chunks fetched under `chunks/` for
  `history -r 1-50` before and after replacing the sequence call; the expected
  drop is one chunk set per revision, the same 2-to-1 change `snapshot_perf.md`
  records for `list -files`.
- Candidate #2: compare `history -threads 1` with `history -threads 8` on a
  storage where a metadata read is a round trip; the output is independent of the
  thread count, so the two runs can be diffed directly, as with `list -threads`.
- Sum the syscalls with the `strace` recipe in `docs/README.md`. On a local
  fixture the storage `openat` count under `chunks/` is the metadata chunks read
  — one unread set per revision is the candidate #1 signature; the
  `.duplicacy/cache/<name>/chunks/` count is the same read served from the cache.
- Compare `history <file>` against `list -files -r 1-50`: both read the same
  per-revision metadata, and the difference is the file-sequence walk of
  candidate #4.
