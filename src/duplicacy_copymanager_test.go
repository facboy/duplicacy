// Copyright (c) Acrosync LLC. All rights reserved.
// Free for personal use and commercial trial
// Commercial use requires per-user licenses available from https://duplicacy.com

package duplicacy

import (
	"bytes"
	"crypto/rsa"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"runtime/debug"
)

// readChunkTree reads every chunk file under '<storageDir>/chunks' into a map from its path relative to 'chunks/' to
// its content, so that two storages can be compared byte for byte.
func readChunkTree(t *testing.T, storageDir string) map[string][]byte {
	tree := make(map[string][]byte)
	root := path.Join(storageDir, "chunks")
	err := filepath.Walk(root, func(chunkPath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || strings.HasSuffix(chunkPath, ".fsl") {
			return nil
		}
		content, err := ioutil.ReadFile(chunkPath)
		if err != nil {
			return err
		}
		relativePath, err := filepath.Rel(root, chunkPath)
		if err != nil {
			return err
		}
		tree[relativePath] = content
		return nil
	})
	if err != nil {
		t.Errorf("Failed to read the chunk tree under %s: %v", root, err)
	}
	return tree
}

// TestIsBitIdenticalWith checks the predicate that decides whether a chunk may be copied without being re-encoded.  A
// pair of storages is bit-identical only when they agree on the chunk hash, the chunk id, the chunk encryption key,
// the compression and the erasure coding, and neither of them uses RSA encryption.
func TestIsBitIdenticalWith(t *testing.T) {

	setTestingT(t)

	base := CreateConfigFromParameters(DEFAULT_COMPRESSION_LEVEL, 64*1024, 256*1024, 16*1024, true, nil, false)

	bitCopy := CreateConfigFromParameters(DEFAULT_COMPRESSION_LEVEL, 64*1024, 256*1024, 16*1024, true, base, true)
	if !base.IsBitIdenticalWith(bitCopy) || !bitCopy.IsBitIdenticalWith(base) {
		t.Errorf("A bit-identical copy should be reported as bit-identical")
	}

	notBitCopy := CreateConfigFromParameters(DEFAULT_COMPRESSION_LEVEL, 64*1024, 256*1024, 16*1024, true, base, false)
	if base.IsBitIdenticalWith(notBitCopy) {
		t.Errorf("A copy that only shares the chunk seed and hash key should not be reported as bit-identical")
	}

	// The compression level changes the stored bytes; 'add -copy -bit-identical' carries it over, so the predicate
	// has to reject a pair that differs even when everything else matches.
	otherCompression := *bitCopy
	otherCompression.CompressionLevel = ZSTD_COMPRESSION_LEVEL_DEFAULT
	if base.IsBitIdenticalWith(&otherCompression) {
		t.Errorf("Storages with different compression levels should not be reported as bit-identical")
	}

	otherChunkKey := *bitCopy
	otherChunkKey.ChunkKey = []byte("a different chunk key for the test")
	if base.IsBitIdenticalWith(&otherChunkKey) {
		t.Errorf("Storages with different chunk keys should not be reported as bit-identical")
	}

	otherIDKey := *bitCopy
	otherIDKey.IDKey = []byte("a different id key for the test")
	if base.IsBitIdenticalWith(&otherIDKey) {
		t.Errorf("Storages with different id keys should not be reported as bit-identical")
	}

	erasureCoding := *bitCopy
	erasureCoding.DataShards = 5
	erasureCoding.ParityShards = 2
	if base.IsBitIdenticalWith(&erasureCoding) {
		t.Errorf("Storages with different erasure-coding settings should not be reported as bit-identical")
	}

	rsaConfig := *bitCopy
	rsaConfig.rsaPublicKey = &rsa.PublicKey{}
	if base.IsBitIdenticalWith(&rsaConfig) {
		t.Errorf("An RSA-encrypted storage should not be reported as bit-identical")
	}

	unencrypted := CreateConfigFromParameters(DEFAULT_COMPRESSION_LEVEL, 64*1024, 256*1024, 16*1024, false, nil, false)
	if !unencrypted.IsBitIdenticalWith(unencrypted) {
		t.Errorf("An unencrypted storage should be bit-identical to itself")
	}
}

// copyCase carries the storages of one copy scenario.
type copyCase struct {
	name        string
	encrypted   bool
	seed        int64
	bitCopy     bool
	rawExpected bool // whether the two storages should store a chunk identically
}

// copyTestStorage is a FileStorage with a settable IsFastListing, counting the lookups, chunk uploads and chunk
// downloads, which is how a test tells how a copy discovered what the destination already held.
type copyTestStorage struct {
	*FileStorage

	isFastListing  bool
	findChunkCalls int64
	uploadedChunks int64
	chunkDownloads int64
}

func (storage *copyTestStorage) IsFastListing() bool { return storage.isFastListing }

func (storage *copyTestStorage) FindChunk(threadIndex int, chunkID string, isFossil bool) (filePath string, exist bool, size int64, err error) {
	atomic.AddInt64(&storage.findChunkCalls, 1)
	return storage.FileStorage.FindChunk(threadIndex, chunkID, isFossil)
}

func (storage *copyTestStorage) UploadFile(threadIndex int, filePath string, content []byte) (err error) {
	if strings.HasPrefix(filePath, "chunks/") {
		atomic.AddInt64(&storage.uploadedChunks, 1)
	}
	return storage.FileStorage.UploadFile(threadIndex, filePath, content)
}

func (storage *copyTestStorage) DownloadFile(threadIndex int, filePath string, chunk *Chunk) (err error) {
	// Chunk files are read both while the snapshots are read and while the chunks are copied; snapshot files are not
	// chunk data and are not counted.
	if strings.HasPrefix(filePath, "chunks/") {
		atomic.AddInt64(&storage.chunkDownloads, 1)
	}
	return storage.FileStorage.DownloadFile(threadIndex, filePath, chunk)
}

// resetCounters clears the counters between operations.
func (storage *copyTestStorage) resetCounters() {
	atomic.StoreInt64(&storage.findChunkCalls, 0)
	atomic.StoreInt64(&storage.uploadedChunks, 0)
	atomic.StoreInt64(&storage.chunkDownloads, 0)
}

// createChunkDirectories creates 'numberOfDirectories' directories under 'chunks/'.
func createChunkDirectories(t *testing.T, storageDir string, numberOfDirectories int) {
	for i := 0; i < numberOfDirectories; i++ {
		dir := path.Join(storageDir, "chunks", fmt.Sprintf("%02x", i))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Errorf("Failed to create the chunk directory %s: %v", dir, err)
			return
		}
	}
}

// TestChunkPath checks that ChunkPath derives the path FindChunk reports for a missing chunk, and rejects an
// inconsistent nesting setup.
func TestChunkPath(t *testing.T) {

	setTestingT(t)

	testDir := path.Join(os.TempDir(), "duplicacy_chunk_path_test")
	os.RemoveAll(testDir)
	os.MkdirAll(testDir, 0700)
	defer os.RemoveAll(testDir)

	chunkID := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	cases := []struct {
		name       string
		readLevels []int
		writeLevel int
		expected   string
	}{
		// The pre-2.0.10 layout of a local storage, where new chunks go to level 2 but older ones may sit at level 3.
		{name: "two levels", readLevels: []int{2, 3}, writeLevel: 2, expected: "chunks/01/23/456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		// The layout of a storage with the fixed nesting.
		{name: "one level", readLevels: []int{1}, writeLevel: 1, expected: "chunks/01/23456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		// A flat storage that keeps all chunks in one directory.
		{name: "no level", readLevels: []int{0}, writeLevel: 0, expected: "chunks/" + chunkID},
	}

	for _, c := range cases {
		storage, err := CreateFileStorage(path.Join(testDir, c.name), false, 1)
		if err != nil {
			t.Errorf("Failed to create the storage of %s: %v", c.name, err)
			continue
		}
		storage.SetDefaultNestingLevels(c.readLevels, c.writeLevel)

		chunkPath, err := storage.ChunkPath(chunkID)
		if err != nil {
			t.Errorf("ChunkPath failed for %s: %v", c.name, err)
			continue
		}
		if chunkPath != c.expected {
			t.Errorf("ChunkPath returned %s instead of %s for %s", chunkPath, c.expected, c.name)
		}

		// The uploader relies on this path when it is told not to look the chunk up.
		foundPath, exist, _, err := storage.FindChunk(0, chunkID, false)
		if err != nil {
			t.Errorf("FindChunk failed for %s: %v", c.name, err)
			continue
		}
		if exist || foundPath != chunkPath {
			t.Errorf("FindChunk returned %s (exist %v) instead of %s for %s", foundPath, exist, chunkPath, c.name)
		}
	}

	// A write level outside the read levels is a broken setup.
	invalidStorage, err := CreateFileStorage(path.Join(testDir, "invalid"), false, 1)
	if err != nil {
		t.Errorf("Failed to create the invalid storage: %v", err)
		return
	}
	invalidStorage.SetDefaultNestingLevels([]int{1}, 2)
	if _, err := invalidStorage.ChunkPath(chunkID); err == nil {
		t.Errorf("ChunkPath should have rejected a write level that isn't in the read levels")
	}
}

// TestCopyChunkProbe covers the two ways a copy learns which chunks the destination already holds: listing it, in
// which case the uploader is not asked to look anything up, or looking each chunk up, which must happen before the
// download so that a chunk already there is never fetched.  Both must leave the second copy of an unchanged
// repository moving nothing.
func TestCopyChunkProbe(t *testing.T) {

	rand.Seed(time.Now().UnixNano())
	setTestingT(t)
	SetLoggingLevel(INFO)

	defer func() {
		if r := recover(); r != nil {
			switch e := r.(type) {
			case Exception:
				t.Errorf("%s %s", e.LogID, e.Message)
				debug.PrintStack()
			default:
				t.Errorf("%v", e)
				debug.PrintStack()
			}
		}
	}()

	testDir := path.Join(os.TempDir(), "duplicacy_copy_probe_test")
	os.RemoveAll(testDir)
	os.MkdirAll(testDir, 0700)
	defer os.RemoveAll(testDir)

	threads := 1
	seed := int64(17)

	sourceDir := path.Join(testDir, "source_storage")
	innerSource, err := loadStorage(sourceDir, threads)
	if err != nil {
		t.Errorf("Failed to create the source storage: %v", err)
		return
	}
	sourceStorage := &copyTestStorage{FileStorage: innerSource.(*FileStorage)}
	if !ConfigStorage(sourceStorage, 16384, DEFAULT_COMPRESSION_LEVEL, 64*1024, 256*1024, 16*1024, "", nil, false, "", 0, 0) {
		t.Errorf("Failed to configure the source storage")
		return
	}

	repository := path.Join(testDir, "repository")
	os.MkdirAll(path.Join(repository, ".duplicacy"), 0700)
	createRandomFileSeeded(path.Join(repository, "file1"), 300000, seed)
	createRandomFileSeeded(path.Join(repository, "file2"), 300000, seed+1)

	SetDuplicacyPreferencePath(path.Join(repository, ".duplicacy"))
	sourceManager := CreateBackupManager("host1", sourceStorage, repository, "", "", "", false)
	sourceManager.SetupSnapshotCache("source")
	if !sourceManager.Backup(repository, true, threads, "first", false, false, 0, false, 1024, 1024) {
		t.Errorf("Failed to back the repository up")
		return
	}

	sourceTree := readChunkTree(t, sourceDir)
	if len(sourceTree) == 0 {
		t.Errorf("Expected the source storage to hold chunks")
		return
	}

	// anyProbes marks an expectation that depends on how the chunk hashes fall into the destination's directories,
	// which the test cannot control: either the destination was listed, or each chunk was looked up once.
	const anyProbes = int64(-1)

	for _, c := range []struct {
		name               string
		isFastListing      bool
		numberOfDirs       int
		expectFirstProbes  int64
		expectSecondProbes int64
	}{
		// A fast-listing storage is listed no matter how many chunk directories it holds, on both copies.
		{name: "listed", isFastListing: true, numberOfDirs: len(sourceTree) + 10,
			expectFirstProbes: 0, expectSecondProbes: 0},
		// A slow-listing storage is listed while its directories are fewer than the chunks to copy, since one
		// listing then costs less than looking every chunk up.  This is the case that must not regress.
		{name: "listed_slow", isFastListing: false, numberOfDirs: 0,
			expectFirstProbes: 0, expectSecondProbes: anyProbes},
		// Once the directories outnumber the chunks to copy, each chunk is looked up instead, before it is
		// downloaded.
		{name: "probed", isFastListing: false, numberOfDirs: len(sourceTree) + 10,
			expectFirstProbes: int64(len(sourceTree)), expectSecondProbes: int64(len(sourceTree))},
	} {
		c := c

		destinationDir := path.Join(testDir, c.name+"_storage")
		innerDestination, err := CreateFileStorage(destinationDir, false, threads)
		if err != nil {
			t.Errorf("Failed to create the %s destination storage: %v", c.name, err)
			continue
		}
		// The count decides whether a slow-listing storage is listed or probed; the names are not chunk ids.
		createChunkDirectories(t, destinationDir, c.numberOfDirs)

		destinationStorage := &copyTestStorage{FileStorage: innerDestination, isFastListing: c.isFastListing}
		if !ConfigStorage(destinationStorage, 16384, DEFAULT_COMPRESSION_LEVEL, 64*1024, 256*1024, 16*1024,
			"", sourceManager.config, false, "", 0, 0) {
			t.Errorf("Failed to configure the %s destination storage", c.name)
			continue
		}

		destinationRepository := path.Join(testDir, c.name+"_repository")
		os.MkdirAll(path.Join(destinationRepository, ".duplicacy"), 0700)
		SetDuplicacyPreferencePath(path.Join(destinationRepository, ".duplicacy"))
		destinationManager := CreateBackupManager("host1", destinationStorage, destinationRepository, "", "", "", false)
		destinationManager.SetupSnapshotCache(c.name)

		// The first copy moves every chunk the snapshot references.
		destinationStorage.resetCounters()
		sourceStorage.resetCounters()

		if !sourceManager.CopySnapshots(destinationManager, "", nil, 1, 1) {
			t.Errorf("Failed to copy the snapshot of %s", c.name)
			continue
		}

		if probes := atomic.LoadInt64(&destinationStorage.findChunkCalls); probes != c.expectFirstProbes {
			t.Errorf("The %s copy made %d per-chunk destination lookups instead of %d",
				c.name, probes, c.expectFirstProbes)
		}
		if uploads := atomic.LoadInt64(&destinationStorage.uploadedChunks); uploads != int64(len(sourceTree)) {
			t.Errorf("The %s copy uploaded %d chunks instead of %d", c.name, uploads, len(sourceTree))
		}
		firstCopyDownloads := atomic.LoadInt64(&sourceStorage.chunkDownloads)
		if firstCopyDownloads < int64(len(sourceTree)) {
			t.Errorf("The %s copy downloaded %d chunks, fewer than the %d it had to move",
				c.name, firstCopyDownloads, len(sourceTree))
		}

		destinationTree := readChunkTree(t, destinationDir)
		if len(destinationTree) != len(sourceTree) {
			t.Errorf("The %s destination holds %d chunks instead of %d", c.name, len(destinationTree), len(sourceTree))
		}
		for chunkPath, content := range sourceTree {
			if destinationContent, found := destinationTree[chunkPath]; !found || !bytes.Equal(content, destinationContent) {
				t.Errorf("The chunk %s of %s was not copied as it is", chunkPath, c.name)
				break
			}
		}

		// The second backup adds a revision referencing the same chunks, so the next copy must move nothing and must
		// not download the chunks to find that out.
		if !sourceManager.Backup(repository, true, threads, "second", false, false, 0, false, 1024, 1024) {
			t.Errorf("Failed to back the repository of %s up again", c.name)
			continue
		}

		destinationStorage.resetCounters()
		sourceStorage.resetCounters()

		if !sourceManager.CopySnapshots(destinationManager, "", nil, 1, 1) {
			t.Errorf("Failed to copy the second snapshot of %s", c.name)
			continue
		}

		if probes := atomic.LoadInt64(&destinationStorage.findChunkCalls); c.expectSecondProbes == anyProbes {
			// Nothing else is a valid outcome.
			if probes != 0 && probes != int64(len(sourceTree)) {
				t.Errorf("The second %s copy made %d per-chunk destination lookups, which is neither a listing nor one lookup per chunk",
					c.name, probes)
			}
		} else if probes != c.expectSecondProbes {
			t.Errorf("The second %s copy made %d per-chunk destination lookups instead of %d",
				c.name, probes, c.expectSecondProbes)
		}
		if uploads := atomic.LoadInt64(&destinationStorage.uploadedChunks); uploads != 0 {
			t.Errorf("The second %s copy uploaded %d chunks although the destination already held them all",
				c.name, uploads)
		}

		// Only metadata is read from the source this time.  The first copy read the metadata too, on top of moving
		// every chunk, so this count has to be the smaller.
		if downloads := atomic.LoadInt64(&sourceStorage.chunkDownloads); downloads >= firstCopyDownloads {
			t.Errorf("The second %s copy downloaded %d chunks, as many as the first one's %d, although it had to move none",
				c.name, downloads, firstCopyDownloads)
		}

		secondDestinationTree := readChunkTree(t, destinationDir)
		if len(secondDestinationTree) != len(destinationTree) {
			t.Errorf("The second %s copy changed the destination from %d to %d chunks", c.name,
				len(destinationTree), len(secondDestinationTree))
		}
	}
}

// TestCopySnapshots copies a snapshot between storages that store chunks identically and between storages that don't,
// and checks that the copy produces a storage that restores to the same files.  For the bit-identical pairs it also
// checks that every destination chunk file is byte-for-byte the source chunk file, which is the point of candidate
// fix #1: the decode/encode round trip is redundant and the stored bytes can be moved as they are.
func TestCopySnapshots(t *testing.T) {

	rand.Seed(time.Now().UnixNano())
	setTestingT(t)
	SetLoggingLevel(INFO)

	defer func() {
		if r := recover(); r != nil {
			switch e := r.(type) {
			case Exception:
				t.Errorf("%s %s", e.LogID, e.Message)
				debug.PrintStack()
			default:
				t.Errorf("%v", e)
				debug.PrintStack()
			}
		}
	}()

	testDir := path.Join(os.TempDir(), "duplicacy_copy_test")
	os.RemoveAll(testDir)
	os.MkdirAll(testDir, 0700)
	defer os.RemoveAll(testDir)

	threads := 1
	password := "duplicacy"

	cases := []copyCase{
		// An unencrypted pair always shares its keys, so the chunks are stored identically.
		{name: "unenc", encrypted: false, seed: 1, bitCopy: false, rawExpected: true},
		// An encrypted pair created with -bit-identical also stores the chunks identically.
		{name: "encbit", encrypted: true, seed: 2, bitCopy: true, rawExpected: true},
		// An encrypted pair without -bit-identical gets fresh keys, so the destination names and ciphertext differ.
		{name: "encfresh", encrypted: true, seed: 3, bitCopy: false, rawExpected: false},
	}

	for _, current := range cases {
		c := current

		storagePassword := ""
		if c.encrypted {
			storagePassword = password
		}

		// Source storage: configure it, create a repository and back it up.
		sourceDir := path.Join(testDir, c.name+"_source_storage")
		sourceStorage, err := loadStorage(sourceDir, threads)
		if err != nil {
			t.Errorf("Failed to create the source storage of %s: %v", c.name, err)
			continue
		}
		cleanStorage(sourceStorage)
		if !ConfigStorage(sourceStorage, 16384, DEFAULT_COMPRESSION_LEVEL, 64*1024, 256*1024, 16*1024,
			storagePassword, nil, false, "", 0, 0) {
			t.Errorf("Failed to configure the source storage of %s", c.name)
			continue
		}

		repository := path.Join(testDir, c.name+"_repository")
		os.MkdirAll(path.Join(repository, ".duplicacy"), 0700)
		createRandomFileSeeded(path.Join(repository, "file1"), 300000, c.seed)
		createRandomFileSeeded(path.Join(repository, "file2"), 300000, c.seed+1)

		SetDuplicacyPreferencePath(path.Join(repository, ".duplicacy"))
		sourceManager := CreateBackupManager("host1", sourceStorage, repository, storagePassword, "", "", false)
		sourceManager.SetupSnapshotCache(c.name)
		if !sourceManager.Backup(repository, true, threads, "first", false, false, 0, false, 1024, 1024) {
			t.Errorf("Failed to back up the repository of %s", c.name)
			continue
		}

		// Destination storage: copy the source configuration, then copy the snapshot.
		destinationDir := path.Join(testDir, c.name+"_destination_storage")
		destinationStorage, err := loadStorage(destinationDir, threads)
		if err != nil {
			t.Errorf("Failed to create the destination storage of %s: %v", c.name, err)
			continue
		}
		cleanStorage(destinationStorage)
		if !ConfigStorage(destinationStorage, 16384, DEFAULT_COMPRESSION_LEVEL, 64*1024, 256*1024, 16*1024,
			storagePassword, sourceManager.config, c.bitCopy, "", 0, 0) {
			t.Errorf("Failed to configure the destination storage of %s", c.name)
			continue
		}

		destinationRepository := path.Join(testDir, c.name+"_destination_repository")
		os.MkdirAll(path.Join(destinationRepository, ".duplicacy"), 0700)
		SetDuplicacyPreferencePath(path.Join(destinationRepository, ".duplicacy"))
		destinationManager := CreateBackupManager("host1", destinationStorage, destinationRepository,
			storagePassword, "", "", false)
		destinationManager.SetupSnapshotCache(c.name + "destination")

		if !sourceManager.CopySnapshots(destinationManager, "", nil, 1, 1) {
			t.Errorf("Failed to copy the snapshot of %s", c.name)
		}

		// The copy must produce a storage from which the original files can be restored.
		restoreDir := path.Join(testDir, c.name+"_restore")
		os.MkdirAll(path.Join(restoreDir, ".duplicacy"), 0700)
		SetDuplicacyPreferencePath(path.Join(restoreDir, ".duplicacy"))
		failedFiles := destinationManager.Restore(restoreDir, 1, true, false, threads, true, false, false, false, nil, false)
		assertRestoreFailures(t, failedFiles, 0)

		for _, file := range []string{"file1", "file2"} {
			sourceHash := getFileHash(path.Join(repository, file))
			restoredHash := getFileHash(path.Join(restoreDir, file))
			if sourceHash != restoredHash {
				t.Errorf("The file %s restored from %s has a different hash: %s vs %s", file, c.name, sourceHash, restoredHash)
			}
		}

		sourceTree := readChunkTree(t, sourceDir)
		destinationTree := readChunkTree(t, destinationDir)

		if len(sourceTree) == 0 {
			t.Errorf("Expected the source storage of %s to hold chunks", c.name)
		}
		if len(sourceTree) != len(destinationTree) {
			t.Errorf("The destination of %s holds %d chunks instead of %d", c.name, len(destinationTree), len(sourceTree))
		}

		identical := 0
		for chunkPath, content := range sourceTree {
			if destinationContent, found := destinationTree[chunkPath]; found && bytes.Equal(content, destinationContent) {
				identical++
			}
		}

		if c.rawExpected {
			if identical != len(sourceTree) {
				t.Errorf("Only %d out of %d chunks of %s were copied byte for byte", identical, len(sourceTree), c.name)
			}
		} else {
			if identical != 0 {
				t.Errorf("Expected the destination chunks of %s to be re-encoded, but %d were identical", c.name, identical)
			}
		}
	}
}
