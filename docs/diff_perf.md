# Where `diff` spends its time

Review of `duplicacy diff`, in the style of `snapshot_perf.md`, `copy_perf.md`,
`prune_perf.md`, `check_perf.md`, `restore_perf.md`, `backup_perf.md`,
`history_perf.md` and `init_perf.md`. This one is a measurement rather than the
code review `history_perf.md` was: the command compares a fixed pair of
revisions, so its metadata cost is bounded by that pair, but the `diff <file>`
form runs a third-party line diff whose cost is quadratic in the length of the
file, the only cost in the command that grows with the payload rather than with
the revision count. Candidate #1 is fixed by a guard that compares the two
revisions by hash above a configurable limit, and says why; candidates #2, #3
and #4 are the three shapes already fixed in `history`, and each is bounded to at
most two revisions here, so they are recorded and not implemented.

## Summary

`diff` compares two revisions of a snapshot, or two revisions of one file. Four
properties cost it work:

1. the `diff <file>` form runs `github.com/aryann/difflib` over both files, whose
   `longestCommonSubsequenceMatrix` allocates one `int` per pair of lines — O(n·m)
   memory and time — and the path also holds both whole files in memory
   (candidate #1) — **Implemented**: above `-max-diff-size` the two revisions are
   compared by hash and the reason is printed;
2. with two revisions and no file argument, both sides expand the chunk-hash
   sequence, which the comparison never reads (candidate #2) — the defect
   `snapshot_perf.md` fix #8 fixed for `list -files`;
3. an explicit `-r` pays the per-revision existence check (candidate #3) — the
   `snapshot_perf.md` fix #3 shape;
4. the two snapshots and their sequences are read one after the other on a
   one-thread operator (candidate #4) — the `snapshot_perf.md` fix #4 shape;
   `diff` has no `-threads` flag.

Candidate #1 is the only one that is not a shape already fixed elsewhere, and the
only one whose cost grows with the payload rather than with the revision count.
Measured on ext4, `diff -r A -r B f.txt` on a 16,000-line file takes 1.8-2.7 s
and 2.1 GB, against 0.11 s for the same revisions without the file argument, and
the resident set tracks `lines × lines × intBytes` exactly. It is fixed by a
guard, not by changing the diff algorithm: above `-max-diff-size` the two
revisions are compared by hash and the reason is printed, so the anchoring of
every diff below the limit is unchanged. Candidates #2, #3 and #4 are each a
small change, but they are the same three shapes `history` fixed and each is
bounded to at most two revisions here — on a cloud storage the whole of candidate
#3 is two round trips and the whole of candidate #4 is one overlap — so they are
recorded and left alone.

## The call path

`diff` (`duplicacy/duplicacy_main.go:1072`) parses the flags, creates the storage
with one thread (`:1087`) — the command has no `-threads` flag (`:1830`) —
and calls `BackupManager.SnapshotManager.Diff` (`:1129`).

`Diff` (`src/duplicacy_snapshotmanager.go:1937`) then does:

```go
manager.CreateChunkOperator(false, false, 1, false)                 // :1944  one thread
defer manager.stopChunkOperator()

if len(revisions) <= 1 {
    rightSnapshot = CreateEmptySnapshot(snapshotID)                 // :1957  the on-disk side
    go rightSnapshot.ListLocalFiles(top, ...)                       // :1961  the local walk
    ... collect rightSnapshotFiles ...
} else {
    rightSnapshot = manager.DownloadSnapshot(snapshotID, revisions[1])  // :1971  listed=false
    manager.DownloadSnapshotSequences(rightSnapshot)                    // :1972  chunks + lengths
}

if len(revisions) < 1 {
    leftSnapshot = manager.downloadLatestSnapshot(snapshotID)       // :1977  already listed
} else {
    leftSnapshot = manager.DownloadSnapshot(snapshotID, revisions[0])   // :1983  listed=false
}
manager.DownloadSnapshotSequences(leftSnapshot)                     // :1986  chunks + lengths

if len(filePath) > 0 {
    RetrieveFile(leftSnapshot, FindFile(leftSnapshot, filePath, false))  // :1990
    RetrieveFile(rightSnapshot, ...) or ioutil.ReadFile(...)             // :2000 / :2009
    ... the guard of candidate #1 ...                                    // :2023-2035
    for _, diff := range difflib.Diff(leftLines, rightLines) { ... }     // :2039
    return true
}

leftSnapshot.ListRemoteFiles(manager.config, manager.chunkOperator, ...)   // :2075
rightSnapshot.ListRemoteFiles(manager.config, manager.chunkOperator, ...)  // :2082
... merge the two sorted lists, comparing Hash ...                        // :2149
```

`DownloadSnapshot` (`:235`) delegates to `downloadSnapshot` with `listed=false`,
which runs `GetFileInfo` before the download (`:254-265`), so the two
`DownloadSnapshot` calls above each pay an existence check. The plural
`DownloadSnapshotSequences` (`:509`) is the pair of `"chunks"` and `"lengths"`
calls, and the singular `DownloadSnapshotSequence(snapshot, "lengths")` (`:479`)
is one of them. `ListRemoteFiles` (`src/duplicacy_snapshot.go:107`) validates each
entry against `snapshot.ChunkLengths` (`:189`) and never reads `ChunkHashes`;
`RetrieveFile` (`src/duplicacy_snapshotmanager.go:1736`) is the only reader,
indexing `snapshot.ChunkHashes[i]` at `:1762`.

## The `diff <file>` line diff is quadratic — candidate #1 — **Implemented**

`Diff` collects both files whole:

```go
var leftFile []byte
manager.RetrieveFile(leftSnapshot, ..., func(content []byte) { leftFile = append(leftFile, content...) })   // :1991
var rightFile []byte
manager.RetrieveFile(rightSnapshot, ..., func(content []byte) { rightFile = append(rightFile, content...) })  // :2001
leftLines := strings.Split(string(leftFile), "\n")     // :2016
rightLines := strings.Split(string(rightFile), "\n")   // :2017
for _, diff := range difflib.Diff(leftLines, rightLines) { ... }   // :2039
```

`difflib.Diff` trims the common head and tail, and then hands what is left to
`compute`, whose first act is `longestCommonSubsequenceMatrix` — an
`intMatrix(len(seq1)+1, len(seq2)+1)`, one Go `int` per pair of surviving lines
(`github.com/aryann/difflib` `difflib.go:142-155`). The table is O(n·m) both in
time and in memory, and it has to exist before a single diff line is printed.

Measured on ext4, one file per revision whose content is entirely different
between the two, `/usr/bin/time -f "%e %M"`, best of three runs:

| lines in each file | `diff -r A -r B <file>` | maxRSS | `diff -r A -r B` |
| --- | --- | --- | --- |
| 2,000 | 0.14 s | 84 MB | 0.11 s |
| 4,000 | 0.23 s | 170 MB | 0.11 s |
| 8,000 | 0.55 s | 557 MB | 0.11 s |
| 16,000 | 1.81 s | 2,100 MB | 0.11 s |

The resident set is the matrix plus a little: intBytes × n² is 32, 128, 512 and
2,048 MB for the four rows, and the measured figures are those plus a process
baseline of 40-50 MB. That figure is the most repeatable of the two, because the
allocation is deterministic: the three runs at 16,000 lines agree to within
0.1%, where the small sizes vary by several percent because the table is not what
dominates. The time has more spread still — 1.8-2.7 s at 16,000 lines across
fixtures — but the quadrupling of the table size at each doubling of the line
count is visible in both, and it is the memory that is the hard limit: the
command is allocating, not waiting. The same command without the file argument
stays flat, because the whole-snapshot form compares hashes and never runs the
line diff. (intBytes is 8 on a 64-bit build and 4 on a 32-bit one; the tables are
half as large there, which is why the guard is described in bytes rather than in
lines.)

`/usr/bin/time -v` at 16,000 lines separates the two: 2.31 s elapsed against 1.25
s user and 0.97 s system time, 96% of the wall clock on the CPU. The system time
is the page faults for the 2 GB the matrix faults in, not I/O.

The cost follows the number of lines that differ, not the size of the file. On the
same 16,000-line pair, `/usr/bin/time`:

| the two 16,000-line revisions differ in | time | maxRSS |
| --- | --- | --- |
| one line | 0.14 s | 79 MB |
| the first 8,000 lines, then all different | 0.76 s | 559 MB |
| every line | 2.28 s | 2,100 MB |

The common head is trimmed, which quarters the table for the middle row: 8,000
surviving lines a side is a quarter the area of 16,000, and 512 MB against
2,048 MB is what the memory column shows. The two halves that do differ still
cost the product of their lengths. Before the guard below, there was no threshold
at which the command degraded gracefully: it allocated the table and then printed,
so peak memory was reached before the first line of output.

**Implemented.** A guard replaces the line diff with the hash comparison above a
configurable amount of memory, and prints the reason. It does not change the diff
algorithm, so the output below the limit is exactly what it was; above it the
command degrades to the same answer the whole-snapshot form gives (`- hash` /
`+ hash`), which is what a reader can act on where the exact diff is not
available at any reasonable cost.

`Diff` takes the limit as `maxDiffBytes` and, before calling `difflib.Diff`,
estimates the table it would build — one `int` for every pair of lines that are
not part of the common head and tail, the same trim `difflib` performs itself
(`commonLineRange`, `lineMatrixExceeds`; `src/duplicacy_snapshotmanager.go`).
Applying the limit after the trim is what keeps the flag from being useless: a
large file with a small change stays on the exact diff, which the measurements
above show is the common case — a 40,000-line file with one line changed is
trimmed to a handful of lines and diffed exactly, having been over 12 GB if the
whole file had been counted. `lineMatrixExceeds` compares against
`maxDiffBytes / intBytes / lines` rather than multiplying, because the product
overflows an `int64` for a file large enough to matter.

The flag is `-max-diff-size` (`duplicacy/duplicacy_main.go:1847`), a size string
through `AtoSize`, defaulting to `256m`; `0` disables the limit, as does a value
`AtoSize` cannot parse, so the flag cannot turn a diff into an error. `256m` is
about 5,800 lines a side on a 64-bit build — comfortably above what a file diff
is meant for, and far below the gigabytes the table would otherwise take.

Measured end to end, on the 6,000-line all-different fixture, `/usr/bin/time`:

```
default (256m)         0.04 s    97 MB    hash comparison, reason printed
-max-diff-size 1000m   0.38 s   337 MB    the exact diff
-max-diff-size 0       0.28 s   338 MB    the exact diff
```

The default run is the guard; a limit above the table (8 × 6,001² = 288 MB, over
256m) restores the quadratic path and its 337 MB, and `0` does the same. On the
40,000-line file with one change, the default keeps the exact diff at 0.04 s and
86 MB, because only the differing lines are counted.

`TestDiffComparesLargeFilesByHash` (`src/duplicacy_diff_perf_test.go`) pins it:
under a 4 KB limit the all-different file prints the reason and the two hashes,
raising the limit above its table restores the line output, and a large file with
a one-line change is diffed line by line under the same 4 KB limit.
`TestDiffWithoutLimitUsesLineDiff` pins the `0` case. Disabling the guard in
`Diff` fails the first with "Diffing a large file should have printed the reason
it was compared by hash".

## The whole-snapshot form expands the chunk-hash sequence it never reads — candidate #2

With two revisions and no file argument, `Diff` calls
`DownloadSnapshotSequences` on both sides (`:1972`, `:1986`), and the comparison
that follows reads only what `ListRemoteFiles` needs: `Entry.check` validates each
entry against `snapshot.ChunkLengths` (`src/duplicacy_snapshot.go:189`), the merge
compares `Hash`, `Size` and `Compare` (`src/duplicacy_snapshotmanager.go:2137`,
`:2149`), and the printed line is `Entry.String` (`src/duplicacy_entry.go:521`).
None of them touches `snapshot.ChunkHashes`. This is `snapshot_perf.md` fix #8,
except that it runs twice — once per side — instead of once per revision.

The per-file branch is not affected: `RetrieveFile` indexes
`snapshot.ChunkHashes[i]` at `:1762`, so `diff <file>` genuinely needs both
sequences.

Measured with `strace -f -e trace=openat`, counting opens under `chunks/` on a
local storage:

```
diff -r 1 -r 5                    6
list -files -r 1 -r 5             4
diff -r 1 -r 5 bigfile.txt        8
```

The two extra opens for the no-file form are the chunk-hash chunk of each side —
one metadata chunk per revision, one round trip on cloud storage and a
`newfstatat` plus `openat` plus the cache write on a local mount, the same
per-chunk saving fix #8 measured for `list -files` (`docs/snapshot_perf.md:311-325`).

The fix is the one `list -files` took, applied to the `filePath == ""` case:
`DownloadSnapshotSequence(snapshot, "lengths")` on each side in place of
`DownloadSnapshotSequences`. The two calls are at the top of the same function, so
the change is two lines and a condition on `filePath`. Recorded; the absolute
saving is two metadata chunks.

## Explicit `-r` pays the existence check — candidate #3

Both `DownloadSnapshot` calls pass `listed=false` (`:1971`, `:1983`), so
`downloadSnapshot` runs `GetFileInfo` before `downloadFile` (`:254-265`). Neither
side was enumerated by a directory listing, so this is not the pure deletion that
`snapshot_perf.md` fix #3 was for `list`: the check is also what reports a named
revision that does not exist as `SNAPSHOT_NOT_EXIST` rather than as a failed
download, and what keeps a stale snapshot cache entry from being believed.

Measured with `strace -f -e trace=newfstatat`, counting calls under
`snapshots/<id>/` on the storage:

```
diff -r 1 -r 5    2
diff -r 5         1
diff              0
```

`diff` with no `-r` pays nothing, because its left side comes from
`downloadLatestSnapshot` (`:1977`), which lists the directory itself and passes
`listed=true` (`:697`). The fix is the one `history` adopted: list the snapshot
directory once and set `listed` when every named revision is present. That
replaces two `GetFileInfo` calls with one listing for `diff -r A -r B`, and one
call with one listing for `diff -r B` — a listing being one round trip against
the one it replaces, so the two-revision case is the only one that gains, and by
a single round trip. Recorded; the smallest of the four.

## Both revisions are read on one thread, one after the other — candidate #4

`Diff` creates its operator with one thread (`:1944`) and calls
`manager.DownloadSnapshot` twice in sequence (`:1971`, `:1983`), then expands the
sequences in sequence (`:1972`, `:1986`). There is no `-threads` flag on the
command, and the storage is created with one thread (`duplicacy_main.go:1087`).
Each snapshot download and each metadata chunk is a round trip on cloud storage,
and on a slow mount each is syscalls; `list` overlaps the first of those with
`downloadSnapshots` (`:881`), and `check`, `restore`, `prune` and `backup` have
since adopted the same shape.

The bound here is two revisions, so the gain is one overlap rather than the
linear-in-revisions gain the other commands get. A fix adds the flag as `history`
did and reuses `downloadSnapshots`. Recorded; not implemented, because a
`-threads` flag that can only ever overlap two units of work is a flag whose
default and maximum are the same.

## Candidate fixes

### Smaller items

- **The on-disk walk is `backup`'s walk.** When the right side is the working
  tree (`diff` and `diff -r N` with no file, `:1954-1969`), `ListLocalFiles`
  (`src/duplicacy_snapshot.go:66`) runs `ListEntries` over the repository, which
  is one `getdents64` loop, one `newfstatat` per entry and one `listxattr` per
  entry (`src/duplicacy_entry.go:708`, `:774`, `src/duplicacy_utils_others.go:53`).
  Measured on a 20,000-file fixture: `diff -r 2` issues 20,000 `listxattr` and a
  matching `newfstatat` per entry, while `diff -r 1 -r 2` — both revisions
  remote — issues none, because no side is a local path. That is the walk
  `backup_perf.md` records as examined and retained, and it is not specific to
  `diff`.
- **`-hash` hashes the working tree, not the snapshots.** On the same fixture,
  `diff -r 2 -hash` took 2.80 s against 0.26 s for `diff -r 2`: `ComputeFileHash`
  reads and hashes every file for which the revision comparison needs a hash
  (`:2147-2149`). That is what the flag promises, as in `history`.
- **The merge itself is linear.** Once both sides are in memory, the two-way
  merge over the sorted entry lists (`:2110-2169`) is one pass, and the
  `maxSize` pre-pass (`:2089-2105`) is two more; neither depends on the file
  contents.
- **The chunk-cache and snapshot-cache decisions are already handled.**
  `prune_perf.md` (option 2) removed the per-write `fsync` from the chunk cache
  and `check_perf.md` made the cache decision filesystem-based, so `diff` — which
  creates its operator with `rewriteChunks` false, like `list` — already benefits.
- **`diff <file>` with a local right side never expands the right sequences.**
  `CreateEmptySnapshot` (`:1957`) has no sequences to expand, and the file is read
  with `ioutil.ReadFile` (`:2009`). Only the left side pays metadata.

### Deliberately not pursued

- **A chunk or revision index.** As in `snapshot_perf.md` (candidate #7),
  `copy_perf.md` and `prune_perf.md`, this would change the storage format on
  disk.
- **A bounded or streaming line diff.** Candidate #1 removed the unbounded memory
  with a guard rather than by changing the algorithm; replacing `difflib` itself,
  which would change the anchoring of the printed diff, is left as the reason
  above records it is not being done.

## How to confirm on a given setup

- `duplicacy -d diff <file>` sets DEBUG logging
  (`duplicacy/duplicacy_main.go:142-148`); `-v` adds TRACE. The
  `DIFF_PARAMETERS` line names the two revisions, each `Downloaded file
  snapshots/<id>/<rev>` is one snapshot download, and `CHUNK_DOWNLOAD` /
  `CHUNK_CACHE` count the metadata chunks behind them.
- Candidate #1: `/usr/bin/time -f "%e %M" duplicacy diff -r A -r B <file>` on a
  large file, and read the maximum resident set; it tracks `lines² × intBytes`.
  `duplicacy -print-memory-usage diff ...`
  (`duplicacy/duplicacy_main.go:150-152`) prints the live heap while it runs, and
  the peak is reached before the first diff line is printed. Compare with `diff -r
  A -r B` without the file argument, which stays flat. A file pair that differs
  everywhere is the worst case; one that shares a long head or tail is trimmed
  before the table is built. The guard is visible as the reason line above the two
  hashes; `-max-diff-size 0` restores the line diff for the same two revisions.
- Candidate #2: `strace -f -e trace=openat duplicacy diff -r A -r B` and count the
  opens under `chunks/` (6 on the fixture), against `duplicacy list -files -r A
  -r B` (4). The difference is the chunk sequence of each side, one chunk per
  revision. The per-file form reads both sequences by design and its count is
  higher for that reason.
- Candidate #3: `strace -f -e trace=newfstatat duplicacy diff -r A -r B` and
  count the calls under `snapshots/<id>/`: two for two named revisions, one for a
  single named revision, none when both revisions were resolved from a listing.
- Candidate #4: not observable on a local fixture, where the whole run is a few
  syscalls. On a storage where a read is a round trip, compare `diff -r A -r B`
  with `list -r A -r B`, which already overlaps its two downloads; the gap is the
  serial pair candidate #4 would remove.
