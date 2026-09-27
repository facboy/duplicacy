# Duplication and refactoring review of the branch

A review of the code added since the branch point, looking for duplicated code and
for refactorings that would make it easier to follow. The baseline is
`upstream/master`, since the two commits on `origin/master` are also part of the
branch.

Line numbers are from the state of the tree when the review was written.

## Summary

The branch is 29 commits ahead of `upstream/master`: about 7,000 added lines over
22 non-test source files, 7 test files, and 6 new documents, plus `AGENTS.md` and
the module files. Almost all of it serves one goal — stop `prune`, `copy` and
`list` paying a round trip per revision, and drop a redundant `fsync`.

The changes work, and the per-backend listing work is factored well. A handful of
shapes nevertheless repeat often enough that a few small helpers would shorten the
code: a concurrent fan-out with panic capture appears four times in three
different idioms, the same recovery block twice verbatim, and snapshot paths are
built in six places.

Ordered by payoff:

1. Concurrency helper — removes ~80–100 lines and one of three competing idioms.
2. Raw-chunk copy plumbing — four flags set from four call sites for one flow.
3. Path and sequence helpers — small, local, mechanical.

## Duplicated code

### Worker fan-out with panic capture — four copies, three idioms

Four places implement "run N workers over a slice, capture the first `LOG_ERROR`
panic, re-raise it in the caller":

- `src/duplicacy_snapshotmanager.go:387-429` (`expandSnapshots`) — atomic counter,
  `WaitGroup`, `failure`/`failureLock`, re-panic.
- `src/duplicacy_snapshotmanager.go:870-913` (`downloadSnapshots`) — the same
  pattern, including the same explanatory comment.
- `src/duplicacy_snapshotmanager.go:2608-2701` (prune snapshot deletion) — the same
  pattern plus the ordered-emission condition variable.
- `src/duplicacy_benchmark.go:64` (`benchmarkRun`) — the older channel-based
  variant.

Three mechanisms coexist for one need inside a single file: `DownloadSequence`
uses `WaitGroup` with `DownloadAsync`, the two new loops use an atomic counter,
and `benchmarkRun` uses channels. A single unexported helper would cover the first
two:

```go
// runConcurrently calls job(threadIndex, index) for each index in [0,count) on
// at most 'threads' goroutines, and re-raises in the caller whatever the first
// worker panicked with.
func runConcurrently(threads, count int, job func(threadIndex, index int))
```

The prune deletion loop needs per-index completion rather than fire-and-forget, so
it should keep its own copy or use a second variant that takes a completion
callback. The other two should not each carry a copy.

### `failure`/`failureLock` and the recovery block — duplicated verbatim

`src/duplicacy_snapshotmanager.go:390-408` and `:873-891` are identical, comment
included. This block should exist once.

### Mapping the three sequences to chunk ids — three implementations

`snapshot.FileSequence`, `ChunkSequence` and `LengthSequence` are walked and mapped
through `config.GetChunkIDFromHash` in:

- `src/duplicacy_snapshotmanager.go:433-443` (`expandSnapshots`, added)
- `src/duplicacy_snapshotmanager.go:767-792` (`GetSnapshotChunks`)
- `src/duplicacy_snapshotmanager.go:804-823` (`GetSnapshotChunkHashes`)
- `src/duplicacy_backupmanager.go:1701-1711` and
  `src/duplicacy_snapshotmanager.go:1016-1020` walk the same fields

`expandSnapshots` re-derives what `GetSnapshotChunks` already does, because it
needs the expansions separated per group. A method on `Snapshot` that yields the
three sequences would give one definition of which chunks a snapshot references,
with `expandChunkHashes` remaining the only variant that downloads.

### Snapshot path building — six copies

`fmt.Sprintf("snapshots/%s/%d", ...)` appears at
`src/duplicacy_backupmanager.go:1140`, `:1659`, `:1882` and
`src/duplicacy_snapshotmanager.go:232`, `:2573`, `:2589`. Two of those are new and
sit ten lines apart inside one function.

`UploadFile` (`src/duplicacy_snapshotmanager.go:3094`) independently slices the
same string to recover the parent directory, so the storage layout now lives in
two places. A `snapshotPath(id, revision)` and `snapshotDir(id)` pair would put it
in one.

### Chunk id recovered from a listed path — seven copies

`strings.Replace(x, "/", "", -1)` at `src/duplicacy_backupmanager.go:203`, `:1764`
and `src/duplicacy_snapshotmanager.go:638`, `:1075`, `:2350`, `:2871`, `:2904`.
The pair "strip the slashes, then strip `.fsl`" appears twice (`:2349-2351`,
`:2871-2872`). A `chunkIDFromListedPath(string) string` helper would make the
fossil and chunk cases read the same.

### OneDrive cloud-file detection — two identical blocks, three files agreeing

`src/duplicacy_chunkmaker.go:177-181` and `:217-220` are identical, comment
included, and the sentinel string is matched again at
`src/duplicacy_backupmanager.go:485`. `isCloudFileError(err) bool` plus a named
constant, or a typed value instead of the string, would make the contract explicit.
As it stands `AddData` carries a third return value that is a magic string
compared by a second call site.

### Missing directory treated as an empty listing — six blocks

`src/duplicacy_dropboxstorage.go:79`, `src/duplicacy_gcdstorage.go:522`,
`src/duplicacy_hubicstorage.go:94`, `src/duplicacy_onestorage.go:104`,
`src/duplicacy_sftpstorage.go:221` and `src/duplicacy_webdavstorage.go:286` each
assert a backend-specific error and return `nil, nil, nil` under a byte-identical
two-line comment.

The assertions have to stay per backend, as the per-backend file layout requires,
so the win is small: move the repeated rationale into one documented helper rather
than repeating it six times.

### `chunkOperator.Stop()` and reassignment — five copies

`src/duplicacy_snapshotmanager.go:926`, `:1041`, `:1769`, `:1994`, `:2124`. A
`deferChunkOperatorStop()` would remove them while the surrounding methods are
already being edited.

### Test scaffolding and storage doubles

Roughly 3,000 lines of test code are added. The shared helpers are scattered by
whoever needed them first: `createTestSnapshotManager`, `uploadRandomChunk`,
`createTestSnapshot` and `uploadTestMetadataChunk` in
`duplicacy_snapshotmanager_test.go`; `readChunkTree` and `createChunkDirectories` in
`duplicacy_copymanager_test.go`; `createRandomFileSeeded` in
`duplicacy_backupmanager_test.go`. The tests call across those files.

Four `FileStorage` wrappers now exist — `copyTestStorage`, `countingStorage`,
`deletionTrackingStorage`, `blockedDownloadStorage` — each overriding a different
subset with its own mutex and counters. The `defer recover()` block that reports
into `t.Errorf` repeats about 13 times. One shared test file and one instrumented
storage base would cut most of it; Go embedding makes this partial, but the current
spread is wider than needed.

### Documents

The six new documents share a fixed shape: title, `## Summary`, `## The call
path`, `## Candidate fixes`, `### Deliberately not pursued`, `## How to confirm on
a given setup`. One `strace` invocation is duplicated verbatim in `copy_perf.md`
and `snapshot_perf.md`. An index page and a stated template would let the set be
read as a whole; `commands.md` also overlaps the wiki table.

## Refactorings, in order of payoff

### Concurrency helper

One unexported helper in `src` covers the fan-out in `expandSnapshots` and
`downloadSnapshots`, and the recovery block that both carry. This is the largest
single reduction and the safest, because the two call sites already behave
identically.

### Raw-chunk copy plumbing

The branch adds `ChunkOperator.skipChunkCheck`, `ChunkOperator.rawData`,
`Chunk.isRawData`, `Chunk.WriteRawData` and `Chunk.SetRawData`, driven from
`src/duplicacy_backupmanager.go:1822`, `:1830`, `:1859` and
`src/duplicacy_chunkoperator.go:457`.

There is a real redundancy: the raw download calls `SetRawData(task.chunkHash)`,
and the copy completion then calls `WriteRawData(chunk.GetBytes(),
chunk.GetHash())`, which calls `SetRawData` a second time. One flag is set through
two mechanisms. Either the downloader should hand over a chunk the uploader
uploads unchanged, or there should be a single constructor for "a stored chunk
copied verbatim" and a single operator flag. Two booleans plus a chunk flag, set
from four call sites, is the part of the branch a reader is most likely to get
wrong.

### Path and sequence helpers

`snapshotPath`/`snapshotDir`, `chunkIDFromListedPath`, and a `Snapshot` accessor
for the three sequences. Each is small and local.

### The string sentinel in `AddData`

Replacing `"CLOUD_FILE_FAILURE"` with a named value makes the three-return
signature self-describing and ties the producer in `duplicacy_chunkmaker.go` to the
consumer in `duplicacy_backupmanager.go`.

## Smaller items

- `src/duplicacy_config.go:172` (`IsCompatibleWith`) and `:187`
  (`IsBitIdenticalWith`) are adjacent predicates over overlapping fields. Both
  compare `HashKey`; the second also compares `IDKey`, `ChunkKey`, compression and
  erasure coding. The comments state the intent, but the two field lists can drift
  apart. Defining one in terms of the other, or sharing a helper, removes that.
- `src/duplicacy_filestorage.go:151`, `:160`, `:165`: `UploadFileNoSync` is a
  `FileStorage` method called directly from `src/duplicacy_chunkoperator.go`, while
  the `Storage` interface exposes only `UploadFile`. The cache durability policy is
  therefore reachable only through the concrete type. That is defensible, since
  only `FileStorage` backs the cache, but the comment explains when to use the
  method rather than why it stays off the interface.
- `src/duplicacy_chunkmaker.go:177-181` returns `-1` as the size on the failure
  path, and the `entry.Size <= 0` check at `src/duplicacy_backupmanager.go:485` is
  what makes that work. The coupling deserves a named sentinel or a comment on the
  backup side.

## Left alone deliberately

- The per-backend `ListFiles` delimiter work in `duplicacy_b2client.go`,
  `duplicacy_b2storage.go` and `duplicacy_azurestorage.go` is already factored
  behind one parameter and is the pattern the rest should follow.
- `ChunkPath` (`src/duplicacy_storage.go:143`) duplicates the path-building loop
  in `FindChunk` (`:161`). The two could share a private helper, but the method and
  its documentation are correct and the duplication is four lines.

## How to apply

Nothing has been changed; this is a review. The recommended order is the
concurrency helper first, then the raw-chunk copy flow, then the path and sequence
helpers. The unit tests in `src/` are the safety net:
`go test ./src/ -vet=off`. Note the two tests that fail on a pristine checkout for
unrelated reasons (`TestEntryExcludeByAttribute`, `TestPersistRestore`), and the
pre-existing `go vet` warnings, both documented in `AGENTS.md`.
