// Copyright (c) Acrosync LLC. All rights reserved.
// Free for personal use and commercial trial
// Commercial use requires per-user licenses available from https://duplicacy.com

package duplicacy

import (
	"bytes"
	"crypto/rsa"
	"io/ioutil"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"strings"
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
