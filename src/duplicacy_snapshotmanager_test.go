// Copyright (c) Acrosync LLC. All rights reserved.
// Free for personal use and commercial trial
// Commercial use requires per-user licenses available from https://duplicacy.com

package duplicacy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack"
)

func createDummySnapshot(snapshotID string, revision int, endTime int64) *Snapshot {
	return &Snapshot{
		ID:       snapshotID,
		Revision: revision,
		EndTime:  endTime,
	}
}

func TestIsDeletable(t *testing.T) {

	//SetLoggingLevel(DEBUG)

	now := time.Now().Unix()
	day := int64(3600 * 24)

	allSnapshots := make(map[string][]*Snapshot)
	allSnapshots["host1"] = append([]*Snapshot{}, createDummySnapshot("host1", 1, now-2*day))
	allSnapshots["host2"] = append([]*Snapshot{}, createDummySnapshot("host2", 1, now-2*day))
	allSnapshots["host1"] = append(allSnapshots["host1"], createDummySnapshot("host1", 2, now-1*day))
	allSnapshots["host2"] = append(allSnapshots["host2"], createDummySnapshot("host2", 2, now-1*day))

	collection := &FossilCollection{
		EndTime:       now - day - 3600,
		LastRevisions: make(map[string]int),
	}

	collection.LastRevisions["host1"] = 1
	collection.LastRevisions["host2"] = 1

	isDeletable, newSnapshots := collection.IsDeletable(true, nil, allSnapshots)
	if !isDeletable || len(newSnapshots) != 2 {
		t.Errorf("Scenario 1: should be deletable, 2 new snapshots")
	}

	collection.LastRevisions["host3"] = 1
	allSnapshots["host3"] = append([]*Snapshot{}, createDummySnapshot("host3", 1, now-2*day))

	isDeletable, newSnapshots = collection.IsDeletable(true, nil, allSnapshots)
	if isDeletable {
		t.Errorf("Scenario 2: should not be deletable")
	}

	allSnapshots["host3"] = append(allSnapshots["host3"], createDummySnapshot("host3", 2, now-day))
	isDeletable, newSnapshots = collection.IsDeletable(true, nil, allSnapshots)
	if !isDeletable || len(newSnapshots) != 3 {
		t.Errorf("Scenario 3: should be deletable, 3 new snapshots")
	}

	collection.LastRevisions["host4"] = 1
	allSnapshots["host4"] = append([]*Snapshot{}, createDummySnapshot("host4", 1, now-8*day))

	isDeletable, newSnapshots = collection.IsDeletable(true, nil, allSnapshots)
	if !isDeletable || len(newSnapshots) != 3 {
		t.Errorf("Scenario 4: should be deletable, 3 new snapshots")
	}

	collection.LastRevisions["repository1@host5"] = 1
	allSnapshots["repository1@host5"] = append([]*Snapshot{}, createDummySnapshot("repository1@host5", 1, now-3*day))

	collection.LastRevisions["repository2@host5"] = 1
	allSnapshots["repository2@host5"] = append([]*Snapshot{}, createDummySnapshot("repository2@host5", 1, now-2*day))

	isDeletable, newSnapshots = collection.IsDeletable(true, nil, allSnapshots)
	if isDeletable {
		t.Errorf("Scenario 5: should not be deletable")
	}

	allSnapshots["repository1@host5"] = append(allSnapshots["repository1@host5"], createDummySnapshot("repository1@host5", 2, now-day))
	isDeletable, newSnapshots = collection.IsDeletable(true, nil, allSnapshots)
	if !isDeletable || len(newSnapshots) != 4 {
		t.Errorf("Scenario 6: should be deletable, 4 new snapshots")
	}
}

func createTestSnapshotManager(testDir string) *SnapshotManager {

	os.RemoveAll(testDir)
	os.MkdirAll(testDir, 0700)

	storage, _ := CreateFileStorage(testDir, false, 1)
	storage.CreateDirectory(0, "chunks")
	storage.CreateDirectory(0, "snapshots")
	config := CreateConfig()
	snapshotManager := CreateSnapshotManager(config, storage)

	cacheDir := path.Join(testDir, "cache")
	snapshotCache, _ := CreateFileStorage(cacheDir, false, 1)
	snapshotCache.CreateDirectory(0, "chunks")
	snapshotCache.CreateDirectory(0, "snapshots")

	snapshotManager.snapshotCache = snapshotCache

	SetDuplicacyPreferencePath(testDir + "/.duplicacy")

	return snapshotManager
}

func uploadTestChunk(manager *SnapshotManager, content []byte) string {

	chunkOperator := CreateChunkOperator(manager.config, manager.storage, nil, false, false, *testThreads, false)
	chunkOperator.UploadCompletionFunc = func(chunk *Chunk, chunkIndex int, skipped bool, chunkSize int, uploadSize int) {
		LOG_INFO("UPLOAD_CHUNK", "Chunk %s size %d uploaded", chunk.GetID(), chunkSize)
	}

	chunk := CreateChunk(manager.config, true)
	chunk.Reset(true)
	chunk.Write(content)

	chunkOperator.Upload(chunk, 0, false)
	chunkOperator.WaitForCompletion()
	chunkOperator.Stop()

	return chunk.GetHash()
}

func uploadRandomChunk(manager *SnapshotManager, chunkSize int) string {
	content := make([]byte, chunkSize)
	_, err := rand.Read(content)
	if err != nil {
		LOG_ERROR("UPLOAD_RANDOM", "Error generating random content: %v", err)
		return ""
	}

	return uploadTestChunk(manager, content)
}

func uploadRandomChunks(manager *SnapshotManager, chunkSize int, numberOfChunks int) []string {
	chunkList := make([]string, 0)
	for i := 0; i < numberOfChunks; i++ {
		chunkHash := uploadRandomChunk(manager, chunkSize)
		chunkList = append(chunkList, chunkHash)
	}
	return chunkList
}

func createTestSnapshot(manager *SnapshotManager, snapshotID string, revision int, startTime int64, endTime int64, chunkHashes []string, tag string) {

	snapshot := &Snapshot{
		ID:          snapshotID,
		Revision:    revision,
		StartTime:   startTime,
		EndTime:     endTime,
		ChunkHashes: chunkHashes,
		Tag:         tag,
	}

	var chunkHashesInHex []string
	for _, chunkHash := range chunkHashes {
		chunkHashesInHex = append(chunkHashesInHex, hex.EncodeToString([]byte(chunkHash)))
	}

	sequence, _ := json.Marshal(chunkHashesInHex)
	snapshot.ChunkSequence = []string{uploadTestChunk(manager, sequence)}

	description, _ := snapshot.MarshalJSON()
	path := fmt.Sprintf("snapshots/%s/%d", snapshotID, snapshot.Revision)
	manager.UploadFile(path, path, description)
}

// uploadTestMetadataChunk uploads 'content' as a metadata chunk, the same way the backup code does for the sequences
// that make up a snapshot.
func uploadTestMetadataChunk(manager *SnapshotManager, content []byte) string {

	chunkOperator := CreateChunkOperator(manager.config, manager.storage, manager.snapshotCache, false, false, 1, false)
	defer chunkOperator.Stop()

	chunkOperator.UploadCompletionFunc = func(chunk *Chunk, chunkIndex int, skipped bool, chunkSize int, uploadSize int) {
	}

	chunk := CreateChunk(manager.config, true)
	chunk.Reset(true)
	chunk.Write(content)

	chunkOperator.Upload(chunk, 0, true)
	chunkOperator.WaitForCompletion()

	return chunk.GetHash()
}

// createTestSnapshotWithFiles uploads a snapshot that lists the given files rather than an empty one, so that the
// commands that walk the file list of a snapshot (list -files) have something to read.  Each file is stored in its own
// chunk, and the entries are encoded the same way BackupManager.UploadSnapshot writes them.  The file hashes that the
// entries carry are returned, so that a test can predict what the file list is printed as.
func createTestSnapshotWithFiles(manager *SnapshotManager, snapshotID string, revision int, startTime int64,
	endTime int64, fileNames []string, fileSizes []int64, tag string) (fileHashes []string) {

	// One chunk per file, with one byte of content per byte of file size.
	chunkHashes := make([]string, len(fileNames))
	chunkLengths := make([]int, len(fileNames))
	fileHashes = make([]string, len(fileNames))

	for i, fileSize := range fileSizes {
		content := make([]byte, fileSize)
		if _, err := rand.Read(content); err != nil {
			LOG_ERROR("SNAPSHOT_UPLOAD", "Failed to generate the content of the file %s: %v", fileNames[i], err)
			return nil
		}
		chunkHashes[i] = uploadTestChunk(manager, content)
		chunkLengths[i] = int(fileSize)

		hasher := manager.config.NewFileHasher()
		hasher.Write(content)
		fileHashes[i] = hex.EncodeToString(hasher.Sum(nil))
	}

	buffer := new(bytes.Buffer)
	encoder := msgpack.NewEncoder(buffer)

	var snapshotChunkHashes []string
	var snapshotChunkLengths []int
	lastChunk := -1
	lastEndChunk := 0

	for i, fileName := range fileNames {

		entry := CreateEntry(fileName, fileSizes[i], startTime, 0644)
		entry.Hash = fileHashes[i]
		entry.StartChunk = i
		entry.StartOffset = 0
		entry.EndChunk = i
		entry.EndOffset = int(fileSizes[i])

		// This mirrors the way UploadSnapshot rewrites the chunk indexes: the chunk indexes of an entry are relative
		// to the previous entry, and each entry only refers to the chunks that weren't referenced before it.
		delta := entry.StartChunk - len(snapshotChunkHashes) + 1
		if entry.StartChunk != lastChunk {
			snapshotChunkHashes = append(snapshotChunkHashes, chunkHashes[entry.StartChunk])
			snapshotChunkLengths = append(snapshotChunkLengths, chunkLengths[entry.StartChunk])
			delta--
		}
		for chunk := entry.StartChunk + 1; chunk <= entry.EndChunk; chunk++ {
			snapshotChunkHashes = append(snapshotChunkHashes, chunkHashes[chunk])
			snapshotChunkLengths = append(snapshotChunkLengths, chunkLengths[chunk])
		}

		lastChunk = entry.EndChunk
		entry.StartChunk -= delta
		entry.EndChunk -= delta

		delta = entry.EndChunk - entry.StartChunk
		entry.StartChunk -= lastEndChunk
		lastEndChunk = entry.EndChunk
		entry.EndChunk = delta

		if err := encoder.Encode(entry); err != nil {
			LOG_ERROR("SNAPSHOT_UPLOAD", "Failed to encode the entry %s: %v", fileName, err)
			return nil
		}
	}

	var totalFileSize int64
	for _, fileSize := range fileSizes {
		totalFileSize += fileSize
	}

	snapshot := &Snapshot{
		Version:       1,
		ID:            snapshotID,
		Revision:      revision,
		StartTime:     startTime,
		EndTime:       endTime,
		Tag:           tag,
		FileSize:      totalFileSize,
		NumberOfFiles: int64(len(fileNames)),
		ChunkHashes:   snapshotChunkHashes,
		ChunkLengths:  snapshotChunkLengths,
	}

	snapshot.FileSequence = []string{uploadTestMetadataChunk(manager, buffer.Bytes())}

	for _, sequenceType := range []string{"chunks", "lengths"} {
		content, err := snapshot.MarshalSequence(sequenceType)
		if err != nil {
			LOG_ERROR("SNAPSHOT_MARSHAL", "Failed to encode the %s in the snapshot: %v", sequenceType, err)
			return nil
		}
		snapshot.SetSequence(sequenceType, []string{uploadTestMetadataChunk(manager, content)})
	}

	description, _ := snapshot.MarshalJSON()
	path := fmt.Sprintf("snapshots/%s/%d", snapshotID, revision)
	manager.UploadFile(path, path, description)

	return fileHashes
}

func checkTestSnapshots(manager *SnapshotManager, expectedSnapshots int, expectedFossils int) {

	manager.CreateChunkOperator(false, false, 1, false)
	defer func() {
		manager.chunkOperator.Stop()
		manager.chunkOperator = nil
	}()

	var snapshotIDs []string
	var err error

	chunks := make(map[string]bool)
	files, _ := manager.ListAllFiles(manager.storage, "chunks/")
	for _, file := range files {
		if file[len(file)-1] == '/' {
			continue
		}
		chunk := strings.Replace(file, "/", "", -1)
		chunks[chunk] = false
	}

	snapshotIDs, err = manager.ListSnapshotIDs()
	if err != nil {
		LOG_ERROR("SNAPSHOT_LIST", "Failed to list all snapshots: %v", err)
		return
	}

	numberOfSnapshots := 0

	for _, snapshotID := range snapshotIDs {

		revisions, err := manager.ListSnapshotRevisions(snapshotID)
		if err != nil {
			LOG_ERROR("SNAPSHOT_LIST", "Failed to list all revisions for snapshot %s: %v", snapshotID, err)
			return
		}

		for _, revision := range revisions {
			snapshot := manager.DownloadSnapshot(snapshotID, revision)
			numberOfSnapshots++

			for _, chunk := range manager.GetSnapshotChunks(snapshot, false) {
				chunks[chunk] = true
			}
		}
	}

	numberOfFossils := 0
	for chunk, referenced := range chunks {
		if !referenced {
			LOG_INFO("UNREFERENCED_CHUNK", "Unreferenced chunk %s", chunk)
			numberOfFossils++
		}
	}

	if numberOfSnapshots != expectedSnapshots {
		LOG_ERROR("SNAPSHOT_COUNT", "Expecting %d snapshots, got %d instead", expectedSnapshots, numberOfSnapshots)
	}

	if numberOfFossils != expectedFossils {
		LOG_ERROR("FOSSIL_COUNT", "Expecting %d unreferenced chunks, got %d instead", expectedFossils, numberOfFossils)
	}
}

// The snapshot cache is only useful for storages that need one; for a local file storage the cached snapshot files
// are never read back, so downloading a snapshot must not write them.
func TestDownloadSnapshotCache(t *testing.T) {

	setTestingT(t)

	testDir := path.Join(os.TempDir(), "duplicacy_test", "snapshot_test")

	snapshotManager := createTestSnapshotManager(testDir)
	storage := snapshotManager.storage.(*FileStorage)

	chunkHash := uploadRandomChunk(snapshotManager, 1024)
	if chunkHash == "" {
		t.Errorf("Failed to upload a chunk")
		return
	}

	now := time.Now().Unix()
	createTestSnapshot(snapshotManager, "vm1@host1", 1, now-3600, now, []string{chunkHash}, "tag")

	cachedSnapshotPath := path.Join(snapshotManager.snapshotCache.storageDir, "snapshots", "vm1@host1", "1")

	if _, err := os.Stat(cachedSnapshotPath); !os.IsNotExist(err) {
		t.Errorf("The snapshot file should not be added to the cache when the storage doesn't need a cache")
	}

	snapshot := snapshotManager.DownloadSnapshot("vm1@host1", 1)
	if snapshot == nil || snapshot.ID != "vm1@host1" || snapshot.Revision != 1 {
		t.Errorf("Failed to download the snapshot vm1@host1 at revision 1")
		return
	}

	if _, err := os.Stat(cachedSnapshotPath); !os.IsNotExist(err) {
		t.Errorf("Downloading a snapshot should not add it to the cache when the storage doesn't need a cache")
	}

	// A storage that needs a cache must still get a copy of every downloaded snapshot file.
	storage.isCacheNeeded = true

	snapshot = snapshotManager.DownloadSnapshot("vm1@host1", 1)
	if snapshot == nil || snapshot.ID != "vm1@host1" || snapshot.Revision != 1 {
		t.Errorf("Failed to download the snapshot vm1@host1 at revision 1")
		return
	}

	if _, err := os.Stat(cachedSnapshotPath); err != nil {
		t.Errorf("The snapshot file should be added to the cache when the storage needs a cache: %v", err)
	}
}

// countingStorage counts the GetFileInfo calls made through the Storage interface, records the thread indexes that
// file downloads were attributed to, and tracks how many downloads are in flight at the same time.  A small delay in
// DownloadFile widens the window in which concurrent downloads overlap, so the peak observed here is what a test uses
// to tell a serial loop from a parallel one.  Methods not overridden here are promoted from the embedded FileStorage,
// so the storage behaves exactly like the real one.
type countingStorage struct {
	*FileStorage
	getFileInfoCalls int
	downloadThreads  map[int]bool
	downloadPaths    map[string]int
	downloadInFlight int
	downloadPeak     int
	downloadLock     sync.Mutex
}

func (storage *countingStorage) GetFileInfo(threadIndex int, filePath string) (exist bool, isDir bool, size int64, err error) {
	storage.getFileInfoCalls++
	return storage.FileStorage.GetFileInfo(threadIndex, filePath)
}

func (storage *countingStorage) DownloadFile(threadIndex int, filePath string, chunk *Chunk) (err error) {

	storage.downloadLock.Lock()
	if storage.downloadThreads == nil {
		storage.downloadThreads = make(map[int]bool)
	}
	if storage.downloadPaths == nil {
		storage.downloadPaths = make(map[string]int)
	}
	storage.downloadThreads[threadIndex] = true
	storage.downloadPaths[filePath]++
	storage.downloadInFlight++
	if storage.downloadInFlight > storage.downloadPeak {
		storage.downloadPeak = storage.downloadInFlight
	}
	storage.downloadLock.Unlock()

	// Give the other workers a chance to enter this method before this download finishes.
	time.Sleep(time.Millisecond)

	err = storage.FileStorage.DownloadFile(threadIndex, filePath, chunk)

	storage.downloadLock.Lock()
	storage.downloadInFlight--
	storage.downloadLock.Unlock()

	return err
}

func (storage *countingStorage) numberOfDownloadThreads() int {
	storage.downloadLock.Lock()
	defer storage.downloadLock.Unlock()
	return len(storage.downloadThreads)
}

// chunkDownloadCounts returns how many times each chunk file was read from the storage.
func (storage *countingStorage) chunkDownloadCounts() (counts map[string]int) {
	storage.downloadLock.Lock()
	defer storage.downloadLock.Unlock()

	counts = make(map[string]int)
	for filePath, count := range storage.downloadPaths {
		if strings.HasPrefix(filePath, "chunks/") {
			counts[filePath] = count
		}
	}
	return counts
}

// resetDownloadStats forgets the thread indexes and the peak number of concurrent downloads seen so far.
func (storage *countingStorage) resetDownloadStats() {
	storage.downloadLock.Lock()
	defer storage.downloadLock.Unlock()
	storage.downloadThreads = nil
	storage.downloadPaths = nil
	storage.downloadPeak = 0
}

// capturedLog is a single message passed to one of the logging functions.
type capturedLog struct {
	level   int
	logID   string
	message string
}

// logCapture records the log messages produced while it is installed as LogFunction, so that a test can inspect what
// a command printed.
type logCapture struct {
	logs     []capturedLog
	logsLock sync.Mutex
}

func (capture *logCapture) log(level int, logID string, message string) {
	capture.logsLock.Lock()
	defer capture.logsLock.Unlock()
	capture.logs = append(capture.logs, capturedLog{level, logID, message})
}

// messages returns the messages that were logged under 'logID'.
func (capture *logCapture) messages(logID string) (messages []string) {
	capture.logsLock.Lock()
	defer capture.logsLock.Unlock()
	for _, log := range capture.logs {
		if log.logID == logID {
			messages = append(messages, log.message)
		}
	}
	return messages
}

// failures returns the messages that were logged as errors, since they are only recorded and never propagated while
// the capture is installed.
func (capture *logCapture) failures() (failures []string) {
	capture.logsLock.Lock()
	defer capture.logsLock.Unlock()
	for _, log := range capture.logs {
		if log.level >= ERROR {
			failures = append(failures, fmt.Sprintf("%s: %s", log.logID, log.message))
		}
	}
	return failures
}

// peakConcurrentDownloads returns the largest number of downloads that were in flight at the same time.
func (storage *countingStorage) peakConcurrentDownloads() int {
	storage.downloadLock.Lock()
	defer storage.downloadLock.Unlock()
	return storage.downloadPeak
}

// Listing snapshots obtains the revisions from ListSnapshotRevisions, which already enumerated the snapshot
// directory of the storage, so checking the existence of every revision again before downloading it only repeats an
// operation that was just performed (and costs a round trip per revision on cloud storages).  The existence check
// must still be performed for revisions that were specified by the user instead of being listed.
func TestDownloadSnapshotSkipsExistenceCheck(t *testing.T) {

	setTestingT(t)

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%v", r)
		}
	}()

	testDir := path.Join(os.TempDir(), "duplicacy_test", "snapshot_test")

	snapshotManager := createTestSnapshotManager(testDir)
	storage := &countingStorage{FileStorage: snapshotManager.storage.(*FileStorage)}
	snapshotManager.storage = storage

	chunkHash := uploadRandomChunk(snapshotManager, 1024)
	if chunkHash == "" {
		t.Errorf("Failed to upload a chunk")
		return
	}

	now := time.Now().Unix()
	createTestSnapshot(snapshotManager, "vm1@host1", 1, now-7200, now-3600, []string{chunkHash}, "tag")
	createTestSnapshot(snapshotManager, "vm1@host1", 2, now-3600, now, []string{chunkHash}, "tag")

	// Listing the revisions of a snapshot id and downloading them must not check their existence individually.
	storage.getFileInfoCalls = 0

	numberOfSnapshots := snapshotManager.ListSnapshots("vm1@host1", []int{}, "", false, false, 1)
	if numberOfSnapshots != 2 {
		t.Errorf("Expecting 2 snapshots, got %d instead", numberOfSnapshots)
	}
	if storage.getFileInfoCalls != 0 {
		t.Errorf("Listing the revisions should not check the existence of each snapshot, but %d checks were made",
			storage.getFileInfoCalls)
	}

	// A revision given by the user wasn't listed, so it is still checked against the storage before the download.
	storage.getFileInfoCalls = 0

	numberOfSnapshots = snapshotManager.ListSnapshots("vm1@host1", []int{1}, "", false, false, 1)
	if numberOfSnapshots != 1 {
		t.Errorf("Expecting 1 snapshot, got %d instead", numberOfSnapshots)
	}
	if storage.getFileInfoCalls != 1 {
		t.Errorf("A revision specified by the user should be checked once, but %d checks were made",
			storage.getFileInfoCalls)
	}

	// The existence check must still be enforced for a revision that is no longer in the storage but whose file is
	// still in the snapshot cache, otherwise a deleted snapshot would remain readable.
	storage.FileStorage.isCacheNeeded = true

	createTestSnapshot(snapshotManager, "vm1@host1", 3, now, now+3600, []string{chunkHash}, "tag")

	cachedSnapshotPath := path.Join(snapshotManager.snapshotCache.storageDir, "snapshots", "vm1@host1", "3")
	if _, err := os.Stat(cachedSnapshotPath); err != nil {
		t.Errorf("Snapshot vm1@host1 at revision 3 should have been added to the cache: %v", err)
		return
	}

	// Delete it from the storage only; the cache keeps a stale copy.
	if err := os.Remove(path.Join(storage.storageDir, "snapshots", "vm1@host1", "3")); err != nil {
		t.Errorf("Failed to delete the snapshot from the storage: %v", err)
		return
	}

	if !downloadedSnapshotMissing(snapshotManager, "vm1@host1", 3) {
		t.Errorf("Downloading the snapshot vm1@host1 at revision 3 should report that it does not exist")
	}
}

// downloadedSnapshotMissing calls DownloadSnapshot and reports whether it reported that the snapshot does not exist,
// which is signaled by LOG_ERROR raising a SNAPSHOT_NOT_EXIST Exception.
func downloadedSnapshotMissing(manager *SnapshotManager, snapshotID string, revision int) (missing bool) {
	// A previous test may have left testingT set, which would turn the expected error into a test failure
	savedTestingT := testingT
	testingT = nil

	defer func() {
		testingT = savedTestingT
		if r := recover(); r != nil {
			if exception, ok := r.(Exception); ok && exception.LogID == "SNAPSHOT_NOT_EXIST" {
				missing = true
				return
			}
			panic(r)
		}
	}()

	manager.DownloadSnapshot(snapshotID, revision)
	return false
}

// Downloading the revisions concurrently must return the same snapshots as downloading them one at a time, in the
// same order, without the workers sharing the download buffer of the manager.
func TestDownloadSnapshotsConcurrently(t *testing.T) {

	setTestingT(t)

	testDir := path.Join(os.TempDir(), "duplicacy_test", "snapshot_test")

	snapshotManager := createTestSnapshotManager(testDir)
	counting := &countingStorage{FileStorage: snapshotManager.storage.(*FileStorage)}
	snapshotManager.storage = counting

	chunkHash := uploadRandomChunk(snapshotManager, 1024)
	if chunkHash == "" {
		t.Errorf("Failed to upload a chunk")
		return
	}

	now := time.Now().Unix()
	for revision := 1; revision <= 8; revision++ {
		createTestSnapshot(snapshotManager, "vm1@host1", revision, now-int64(revision)*3600, now, []string{chunkHash}, "tag")
	}

	revisions, err := snapshotManager.ListSnapshotRevisions("vm1@host1")
	if err != nil {
		t.Errorf("Failed to list the revisions: %v", err)
		return
	}
	if len(revisions) != 8 {
		t.Errorf("Expecting 8 revisions, got %d", len(revisions))
		return
	}

	// The revisions must come back in order, each carrying its own revision number, no matter how many workers
	// downloaded them and in which order they finished.
	for _, threads := range []int{1, 2, 4, 16} {
		counting.resetDownloadStats()

		snapshots := snapshotManager.downloadSnapshots("vm1@host1", revisions, true, threads)
		if len(snapshots) != len(revisions) {
			t.Errorf("With %d threads: expecting %d snapshots, got %d", threads, len(revisions), len(snapshots))
			continue
		}
		for i, revision := range revisions {
			snapshot := snapshots[i]
			if snapshot == nil {
				t.Errorf("With %d threads: the snapshot at revision %d was not downloaded", threads, revision)
				continue
			}
			if snapshot.Revision != revision {
				t.Errorf("With %d threads: expecting revision %d at position %d, got %d",
					threads, revision, i, snapshot.Revision)
			}
		}

		// Each snapshot is a separate download attributed to the worker that handled it, and the downloads must be
		// spread over the thread indexes the storage was told to expect -- but never beyond them, since some
		// backends index a per-thread client or nested directory with the thread index.
		usedThreads := counting.numberOfDownloadThreads()
		if usedThreads > threads {
			t.Errorf("With %d threads: the storage saw %d different thread indexes, more than the storage was created with",
				threads, usedThreads)
		}

		// The downloads must really overlap: a serial loop never has two of them in flight at the same time.
		peak := counting.peakConcurrentDownloads()
		if threads == 1 && peak != 1 {
			t.Errorf("With 1 thread: expecting at most one download at a time, got %d", peak)
		}
		if threads > 1 && peak < 2 {
			t.Errorf("With %d threads: the downloads did not overlap, at most %d was in flight at a time", threads, peak)
		}
	}

	// The number of snapshots reported by list must not depend on the number of threads either.
	if numberOfSnapshots := snapshotManager.ListSnapshots("vm1@host1", []int{}, "", false, false, 4); numberOfSnapshots != 8 {
		t.Errorf("Expecting 8 snapshots from a concurrent list, got %d", numberOfSnapshots)
	}
}

// 'list -files' printed the file list by walking the file sequence twice: once to compute the total size and the width
// of the size column, and once to print the entries.  Each walk downloads every metadata chunk of the sequence again,
// so the second one is pure overhead (and a round trip per chunk on cloud storage).  The file list must instead be
// produced from a single walk, with the same output as before.
func TestListFilesWalksTheFileSequenceOnce(t *testing.T) {

	setTestingT(t)

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%v", r)
		}
	}()

	testDir := path.Join(os.TempDir(), "duplicacy_test", "snapshot_test")

	snapshotManager := createTestSnapshotManager(testDir)
	counting := &countingStorage{FileStorage: snapshotManager.storage.(*FileStorage)}
	snapshotManager.storage = counting

	// The entries are deliberately not in sorted order: the order they are printed in must be the order they are
	// stored in, which is how the backup writes them.
	fileNames := []string{"file1", "file2", "file10", "dir1/file3"}
	fileSizes := []int64{9, 1234, 12345678, 10}

	now := time.Now().Unix()
	fileHashes := createTestSnapshotWithFiles(snapshotManager, "vm1@host1", 1, now-3600, now, fileNames, fileSizes, "tag")

	// Capture what list prints instead of letting it go to the test log.
	savedLogFunction := LogFunction
	capture := &logCapture{}
	LogFunction = capture.log
	defer func() {
		LogFunction = savedLogFunction
	}()

	counting.resetDownloadStats()

	if numberOfSnapshots := snapshotManager.ListSnapshots("vm1@host1", []int{}, "", true, false, 1); numberOfSnapshots != 1 {
		t.Errorf("Expecting 1 snapshot, got %d", numberOfSnapshots)
	}

	if failures := capture.failures(); len(failures) > 0 {
		t.Errorf("Listing the files of the snapshot failed: %v", failures)
		return
	}

	// Every metadata chunk of the snapshot must be fetched exactly once: the file sequence, the chunk sequence and the
	// length sequence.  A chunk that is fetched a second time is served from the snapshot cache rather than from the
	// storage, so the downloads and the cache hits are counted together; walking the file sequence twice adds a cache
	// hit for its chunk, which is exactly the overhead this fix removes.
	fetches := len(capture.messages("CHUNK_DOWNLOAD")) + len(capture.messages("CHUNK_CACHE"))
	if fetches != 3 {
		t.Errorf("Expecting the 3 metadata chunks of the snapshot to be fetched once each, got %d fetches", fetches)
	}

	downloads := counting.chunkDownloadCounts()
	if len(downloads) != 3 {
		t.Errorf("Expecting the 3 metadata chunks of the snapshot to be downloaded, got %v", downloads)
	}
	for chunkPath, count := range downloads {
		if count != 1 {
			t.Errorf("The metadata chunk %s was downloaded %d times instead of once", chunkPath, count)
		}
	}

	// The printed file list must contain every file, in the order the entries are stored, with the size column
	// widened to the largest file.  The width follows the rule used by list: it grows a digit at a time while a
	// larger size is found.
	maxSize := int64(9)
	maxSizeDigits := 1
	for _, fileSize := range fileSizes {
		if fileSize > maxSize {
			maxSize = maxSize*10 + 9
			maxSizeDigits++
		}
	}
	modifiedTime := time.Unix(now-3600, 0).Format("2006-01-02 15:04:05")

	files := capture.messages("SNAPSHOT_FILE")
	if len(files) != len(fileNames) {
		t.Errorf("Expecting %d files to be printed, got %d: %v", len(fileNames), len(files), files)
		return
	}

	for i, fileName := range fileNames {
		expectedFile := fmt.Sprintf("%*d %s %s %s", maxSizeDigits, fileSizes[i], modifiedTime, fileHashes[i], fileName)
		if files[i] != expectedFile {
			t.Errorf("Expecting the file %s to be printed as %q, got %q", fileName, expectedFile, files[i])
		}
	}

	// The statistics computed from the same walk must still be printed.
	stats := capture.messages("SNAPSHOT_STATS")
	if len(stats) != 2 {
		t.Errorf("Expecting the file count and the sizes to be printed, got %v", stats)
		return
	}
	if stats[0] != fmt.Sprintf("Files: %d", len(fileNames)) {
		t.Errorf("Expecting %q, got %q", fmt.Sprintf("Files: %d", len(fileNames)), stats[0])
	}
	expectedTotalSize := int64(9 + 1234 + 12345678 + 10)
	expectedStats := fmt.Sprintf("Total size: %d, file chunks: 4, metadata chunks: 3", expectedTotalSize)
	if stats[1] != expectedStats {
		t.Errorf("Expecting %q, got %q", expectedStats, stats[1])
	}
}

// Reading snapshots must never create directories.  The snapshot directory is created by the code that uploads a
// snapshot, so read-only commands (list, check, cat, ...) leave the storage and the snapshot cache untouched.
func TestReadSnapshotsDoesNotCreateDirectories(t *testing.T) {

	setTestingT(t)

	testDir := path.Join(os.TempDir(), "duplicacy_test", "snapshot_test")

	snapshotManager := createTestSnapshotManager(testDir)
	storageDir := snapshotManager.storage.(*FileStorage).storageDir
	cacheDir := snapshotManager.snapshotCache.storageDir

	nonexistentStorageDir := path.Join(storageDir, "snapshots", "vm1@host1")
	nonexistentCacheDir := path.Join(cacheDir, "snapshots", "vm1@host1")

	// Listing the revisions of a snapshot id that has never been backed up reports no revisions, and must not
	// create the snapshot directory on the storage or in the snapshot cache.
	revisions, err := snapshotManager.ListSnapshotRevisions("vm1@host1")
	if err != nil {
		t.Errorf("Failed to list the revisions of a nonexistent snapshot: %v", err)
		return
	}
	if len(revisions) != 0 {
		t.Errorf("Expecting no revisions for a nonexistent snapshot, got %d", len(revisions))
	}
	if _, err := os.Stat(nonexistentStorageDir); !os.IsNotExist(err) {
		t.Errorf("Listing the revisions should not create the snapshot directory in the storage")
	}
	if _, err := os.Stat(nonexistentCacheDir); !os.IsNotExist(err) {
		t.Errorf("Listing the revisions should not create the snapshot directory in the snapshot cache")
	}

	// Uploading a snapshot is what creates the snapshot directory.
	chunkHash := uploadRandomChunk(snapshotManager, 1024)
	if chunkHash == "" {
		t.Errorf("Failed to upload a chunk")
		return
	}

	now := time.Now().Unix()
	createTestSnapshot(snapshotManager, "vm1@host1", 1, now-3600, now, []string{chunkHash}, "tag")

	if _, err := os.Stat(nonexistentStorageDir); err != nil {
		t.Errorf("Uploading a snapshot should create the snapshot directory: %v", err)
	}

	// Downloading it back must again not touch the snapshot cache, since a file storage doesn't need a cache.
	snapshot := snapshotManager.DownloadSnapshot("vm1@host1", 1)
	if snapshot == nil || snapshot.ID != "vm1@host1" || snapshot.Revision != 1 {
		t.Errorf("Failed to download the snapshot vm1@host1 at revision 1")
		return
	}

	if _, err := os.Stat(nonexistentCacheDir); !os.IsNotExist(err) {
		t.Errorf("Downloading a snapshot should not create the snapshot directory in the snapshot cache")
	}
}

func TestPruneSingleRepository(t *testing.T) {

	setTestingT(t)

	testDir := path.Join(os.TempDir(), "duplicacy_test", "snapshot_test")

	snapshotManager := createTestSnapshotManager(testDir)

	chunkSize := 1024
	chunkHash1 := uploadRandomChunk(snapshotManager, chunkSize)
	chunkHash2 := uploadRandomChunk(snapshotManager, chunkSize)
	chunkHash3 := uploadRandomChunk(snapshotManager, chunkSize)
	chunkHash4 := uploadRandomChunk(snapshotManager, chunkSize)

	now := time.Now().Unix()
	day := int64(24 * 3600)
	t.Logf("Creating 2 snapshots")
	createTestSnapshot(snapshotManager, "repository1", 1, now-4*day-3600, now-3*day-60, []string{chunkHash1, chunkHash2}, "tag")
	createTestSnapshot(snapshotManager, "repository1", 2, now-4*day-3600, now-3*day-60, []string{chunkHash1, chunkHash2}, "tag")
	checkTestSnapshots(snapshotManager, 2, 2)

	t.Logf("Creating 2 snapshots")
	createTestSnapshot(snapshotManager, "repository1", 3, now-2*day-3600, now-2*day-60, []string{chunkHash2, chunkHash3}, "tag")
	createTestSnapshot(snapshotManager, "repository1", 4, now-1*day-3600, now-1*day-60, []string{chunkHash3, chunkHash4}, "tag")
	checkTestSnapshots(snapshotManager, 4, 0)

	t.Logf("Removing snapshot repository1 revisions 1 and 2 with --exclusive")
	snapshotManager.PruneSnapshots("repository1", "repository1", []int{1, 2}, []string{}, []string{}, false, true, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 2, 0)

	t.Logf("Removing snapshot repository1 revision 3 without --exclusive")
	snapshotManager.PruneSnapshots("repository1", "repository1", []int{3}, []string{}, []string{}, false, false, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 1, 2)

	t.Logf("Creating 1 snapshot")
	chunkHash5 := uploadRandomChunk(snapshotManager, chunkSize)
	createTestSnapshot(snapshotManager, "repository1", 5, now+1*day-3600, now+1*day, []string{chunkHash4, chunkHash5}, "tag")
	checkTestSnapshots(snapshotManager, 2, 2)

	t.Logf("Prune without removing any snapshots -- fossils will be deleted")
	snapshotManager.PruneSnapshots("repository1", "repository1", []int{}, []string{}, []string{}, false, false, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 2, 0)
}

func TestPruneSingleHost(t *testing.T) {

	setTestingT(t)

	testDir := path.Join(os.TempDir(), "duplicacy_test", "snapshot_test")

	snapshotManager := createTestSnapshotManager(testDir)

	chunkSize := 1024
	chunkHash1 := uploadRandomChunk(snapshotManager, chunkSize)
	chunkHash2 := uploadRandomChunk(snapshotManager, chunkSize)
	chunkHash3 := uploadRandomChunk(snapshotManager, chunkSize)
	chunkHash4 := uploadRandomChunk(snapshotManager, chunkSize)

	now := time.Now().Unix()
	day := int64(24 * 3600)
	t.Logf("Creating 3 snapshots")
	createTestSnapshot(snapshotManager, "vm1@host1", 1, now-3*day-3600, now-3*day-60, []string{chunkHash1, chunkHash2}, "tag")
	createTestSnapshot(snapshotManager, "vm1@host1", 2, now-2*day-3600, now-2*day-60, []string{chunkHash2, chunkHash3}, "tag")
	createTestSnapshot(snapshotManager, "vm2@host1", 1, now-3*day-3600, now-3*day-60, []string{chunkHash3, chunkHash4}, "tag")
	checkTestSnapshots(snapshotManager, 3, 0)

	t.Logf("Removing snapshot vm1@host1 revision 1 without --exclusive")
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{1}, []string{}, []string{}, false, false, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 2, 2)

	t.Logf("Prune without removing any snapshots -- no fossils will be deleted")
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{}, []string{}, []string{}, false, false, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 2, 2)

	t.Logf("Creating 1 snapshot")
	chunkHash5 := uploadRandomChunk(snapshotManager, chunkSize)
	createTestSnapshot(snapshotManager, "vm2@host1", 2, now+1*day-3600, now+1*day, []string{chunkHash4, chunkHash5}, "tag")
	checkTestSnapshots(snapshotManager, 3, 2)

	t.Logf("Prune without removing any snapshots -- fossils will be deleted")
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{}, []string{}, []string{}, false, false, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 3, 0)

}

func TestPruneMultipleHost(t *testing.T) {

	setTestingT(t)

	testDir := path.Join(os.TempDir(), "duplicacy_test", "snapshot_test")

	snapshotManager := createTestSnapshotManager(testDir)

	chunkSize := 1024
	chunkHash1 := uploadRandomChunk(snapshotManager, chunkSize)
	chunkHash2 := uploadRandomChunk(snapshotManager, chunkSize)
	chunkHash3 := uploadRandomChunk(snapshotManager, chunkSize)
	chunkHash4 := uploadRandomChunk(snapshotManager, chunkSize)

	now := time.Now().Unix()
	day := int64(24 * 3600)
	t.Logf("Creating 3 snapshot")
	createTestSnapshot(snapshotManager, "vm1@host1", 1, now-3*day-3600, now-3*day-60, []string{chunkHash1, chunkHash2}, "tag")
	createTestSnapshot(snapshotManager, "vm1@host1", 2, now-2*day-3600, now-2*day-60, []string{chunkHash2, chunkHash3}, "tag")
	createTestSnapshot(snapshotManager, "vm2@host2", 1, now-3*day-3600, now-3*day-60, []string{chunkHash3, chunkHash4}, "tag")
	checkTestSnapshots(snapshotManager, 3, 0)

	t.Logf("Removing snapshot vm1@host1 revision 1 without --exclusive")
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{1}, []string{}, []string{}, false, false, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 2, 2)

	t.Logf("Prune without removing any snapshots -- no fossils will be deleted")
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{}, []string{}, []string{}, false, false, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 2, 2)

	t.Logf("Creating 1 snapshot")
	chunkHash5 := uploadRandomChunk(snapshotManager, chunkSize)
	createTestSnapshot(snapshotManager, "vm2@host2", 2, now+1*day-3600, now+1*day, []string{chunkHash4, chunkHash5}, "tag")
	checkTestSnapshots(snapshotManager, 3, 2)

	t.Logf("Prune without removing any snapshots -- no fossils will be deleted")
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{}, []string{}, []string{}, false, false, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 3, 2)

	t.Logf("Creating 1 snapshot")
	chunkHash6 := uploadRandomChunk(snapshotManager, chunkSize)
	createTestSnapshot(snapshotManager, "vm1@host1", 3, now+1*day-3600, now+1*day, []string{chunkHash5, chunkHash6}, "tag")
	checkTestSnapshots(snapshotManager, 4, 2)

	t.Logf("Prune without removing any snapshots -- fossils will be deleted")
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{}, []string{}, []string{}, false, false, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 4, 0)
}

func TestPruneAndResurrect(t *testing.T) {

	setTestingT(t)

	testDir := path.Join(os.TempDir(), "duplicacy_test", "snapshot_test")

	snapshotManager := createTestSnapshotManager(testDir)

	chunkSize := 1024
	chunkHash1 := uploadRandomChunk(snapshotManager, chunkSize)
	chunkHash2 := uploadRandomChunk(snapshotManager, chunkSize)
	chunkHash3 := uploadRandomChunk(snapshotManager, chunkSize)

	now := time.Now().Unix()
	day := int64(24 * 3600)
	t.Logf("Creating 2 snapshots")
	createTestSnapshot(snapshotManager, "vm1@host1", 1, now-3*day-3600, now-3*day-60, []string{chunkHash1, chunkHash2}, "tag")
	createTestSnapshot(snapshotManager, "vm1@host1", 2, now-2*day-3600, now-2*day-60, []string{chunkHash2, chunkHash3}, "tag")
	checkTestSnapshots(snapshotManager, 2, 0)

	t.Logf("Removing snapshot vm1@host1 revision 1 without --exclusive")
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{1}, []string{}, []string{}, false, false, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 1, 2)

	t.Logf("Creating 1 snapshot")
	chunkHash4 := uploadRandomChunk(snapshotManager, chunkSize)
	createTestSnapshot(snapshotManager, "vm1@host1", 4, now+1*day-3600, now+1*day, []string{chunkHash4, chunkHash1}, "tag")
	checkTestSnapshots(snapshotManager, 2, 2)

	t.Logf("Prune without removing any snapshots -- one fossil will be resurrected")
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{}, []string{}, []string{}, false, false, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 2, 0)
}

func TestPruneWithInactiveHost(t *testing.T) {

	setTestingT(t)

	testDir := path.Join(os.TempDir(), "duplicacy_test", "snapshot_test")

	snapshotManager := createTestSnapshotManager(testDir)

	chunkSize := 1024
	chunkHash1 := uploadRandomChunk(snapshotManager, chunkSize)
	chunkHash2 := uploadRandomChunk(snapshotManager, chunkSize)
	chunkHash3 := uploadRandomChunk(snapshotManager, chunkSize)
	chunkHash4 := uploadRandomChunk(snapshotManager, chunkSize)

	now := time.Now().Unix()
	day := int64(24 * 3600)
	t.Logf("Creating 3 snapshot")
	createTestSnapshot(snapshotManager, "vm1@host1", 1, now-3*day-3600, now-3*day-60, []string{chunkHash1, chunkHash2}, "tag")
	createTestSnapshot(snapshotManager, "vm1@host1", 2, now-2*day-3600, now-2*day-60, []string{chunkHash2, chunkHash3}, "tag")
	// Host2 is inactive
	createTestSnapshot(snapshotManager, "vm2@host2", 1, now-7*day-3600, now-7*day-60, []string{chunkHash3, chunkHash4}, "tag")
	checkTestSnapshots(snapshotManager, 3, 0)

	t.Logf("Removing snapshot vm1@host1 revision 1")
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{1}, []string{}, []string{}, false, false, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 2, 2)

	t.Logf("Prune without removing any snapshots -- no fossils will be deleted")
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{}, []string{}, []string{}, false, false, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 2, 2)

	t.Logf("Creating 1 snapshot")
	chunkHash5 := uploadRandomChunk(snapshotManager, chunkSize)
	createTestSnapshot(snapshotManager, "vm1@host1", 3, now+1*day-3600, now+1*day, []string{chunkHash4, chunkHash5}, "tag")
	checkTestSnapshots(snapshotManager, 3, 2)

	t.Logf("Prune without removing any snapshots -- fossils will be deleted")
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{}, []string{}, []string{}, false, false, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 3, 0)
}

func TestPruneWithRetentionPolicy(t *testing.T) {

	setTestingT(t)

	testDir := path.Join(os.TempDir(), "duplicacy_test", "snapshot_test")

	snapshotManager := createTestSnapshotManager(testDir)

	chunkSize := 1024
	var chunkHashes []string
	for i := 0; i < 30; i++ {
		chunkHashes = append(chunkHashes, uploadRandomChunk(snapshotManager, chunkSize))
	}

	now := time.Now().Unix()
	day := int64(24 * 3600)
	t.Logf("Creating 30 snapshots")
	for i := 0; i < 30; i++ {
		createTestSnapshot(snapshotManager, "vm1@host1", i+1, now-int64(30-i)*day-3600, now-int64(30-i)*day-60, []string{chunkHashes[i]}, "tag")
	}

	checkTestSnapshots(snapshotManager, 30, 0)

	t.Logf("Removing snapshot vm1@host1 0:20 with --exclusive")
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{}, []string{}, []string{"0:20"}, false, true, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 19, 0)

	t.Logf("Removing snapshot vm1@host1 -k 0:20 with --exclusive")
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{}, []string{}, []string{"0:20"}, false, true, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 19, 0)

	t.Logf("Removing snapshot vm1@host1 -k 3:14 -k 2:7 with --exclusive")
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{}, []string{}, []string{"3:14", "2:7"}, false, true, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 12, 0)
}

func TestPruneWithRetentionPolicyAndTag(t *testing.T) {

	setTestingT(t)

	testDir := path.Join(os.TempDir(), "duplicacy_test", "snapshot_test")

	snapshotManager := createTestSnapshotManager(testDir)

	chunkSize := 1024
	var chunkHashes []string
	for i := 0; i < 30; i++ {
		chunkHashes = append(chunkHashes, uploadRandomChunk(snapshotManager, chunkSize))
	}

	now := time.Now().Unix()
	day := int64(24 * 3600)
	t.Logf("Creating 30 snapshots")
	for i := 0; i < 30; i++ {
		tag := "auto"
		if i%3 == 0 {
			tag = "manual"
		}
		createTestSnapshot(snapshotManager, "vm1@host1", i+1, now-int64(30-i)*day-3600, now-int64(30-i)*day-60, []string{chunkHashes[i]}, tag)
	}

	checkTestSnapshots(snapshotManager, 30, 0)

	t.Logf("Removing snapshot vm1@host1 0:20 with --exclusive and --tag manual")
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{}, []string{"manual"}, []string{"0:7"}, false, true, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 22, 0)
}

// Test that an unreferenced fossil shouldn't be removed as it may be the result of another prune job in-progress.
func TestPruneWithFossils(t *testing.T) {
	setTestingT(t)

	testDir := path.Join(os.TempDir(), "duplicacy_test", "snapshot_test")

	snapshotManager := createTestSnapshotManager(testDir)

	chunkSize := 1024
	chunkHash1 := uploadRandomChunk(snapshotManager, chunkSize)
	chunkHash2 := uploadRandomChunk(snapshotManager, chunkSize)
	chunkHash3 := uploadRandomChunk(snapshotManager, chunkSize)
	// Create an unreferenced fossil
	snapshotManager.storage.UploadFile(0, "chunks/113b6a2350dcfd836829c47304dd330fa6b58b93dd7ac696c6b7b913e6868662.fsl", []byte("this is a test fossil"))

	now := time.Now().Unix()
	day := int64(24 * 3600)
	t.Logf("Creating 2 snapshots")
	createTestSnapshot(snapshotManager, "vm1@host1", 1, now-3*day-3600, now-3*day-60, []string{chunkHash1, chunkHash2}, "tag")
	createTestSnapshot(snapshotManager, "vm1@host1", 2, now-2*day-3600, now-2*day-60, []string{chunkHash2, chunkHash3}, "tag")
	checkTestSnapshots(snapshotManager, 2, 1)

	t.Logf("Prune without removing any snapshots but with --exhaustive")
	// The unreferenced fossil shouldn't be removed
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{}, []string{}, []string{}, true, false, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 2, 1)

	t.Logf("Prune without removing any snapshots but with --exclusive")
	// Now the unreferenced fossil should be removed
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{}, []string{}, []string{}, false, true, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 2, 0)
}

func TestPruneMultipleThread(t *testing.T) {

	setTestingT(t)

	testDir := path.Join(os.TempDir(), "duplicacy_test", "snapshot_test")

	snapshotManager := createTestSnapshotManager(testDir)

	chunkSize := 1024
	numberOfChunks := 256
	numberOfThreads := 4

	chunkList1 := uploadRandomChunks(snapshotManager, chunkSize, numberOfChunks)
	chunkList2 := uploadRandomChunks(snapshotManager, chunkSize, numberOfChunks)

	now := time.Now().Unix()
	day := int64(24 * 3600)
	t.Logf("Creating 2 snapshots")
	createTestSnapshot(snapshotManager, "repository1", 1, now-4*day-3600, now-3*day-60, chunkList1, "tag")
	createTestSnapshot(snapshotManager, "repository1", 2, now-3*day-3600, now-2*day-60, chunkList2, "tag")
	checkTestSnapshots(snapshotManager, 2, 0)

	t.Logf("Removing snapshot revisions 1 with --exclusive")
	snapshotManager.PruneSnapshots("repository1", "repository1", []int{1}, []string{}, []string{}, false, true, []string{}, false, false, false, numberOfThreads)
	checkTestSnapshots(snapshotManager, 1, 0)

	t.Logf("Creating 1 more snapshot")
	chunkList3 := uploadRandomChunks(snapshotManager, chunkSize, numberOfChunks)
	createTestSnapshot(snapshotManager, "repository1", 3, now-2*day-3600, now-1*day-60, chunkList3, "tag")

	t.Logf("Removing snapshot repository1 revision 2 without --exclusive")
	snapshotManager.PruneSnapshots("repository1", "repository1", []int{2}, []string{}, []string{}, false, false, []string{}, false, false, false, numberOfThreads)

	t.Logf("Prune without removing any snapshots but with --exclusive")
	snapshotManager.PruneSnapshots("repository1", "repository1", []int{}, []string{}, []string{}, false, true, []string{}, false, false, false, numberOfThreads)
	checkTestSnapshots(snapshotManager, 1, 0)
}

// Unlike the chunk cache, the cached snapshot files, fossil collections and verified-chunk list are read back
// without any check on the content, so those writes must stay durable -- which is why only the chunk cache is
// written through FileStorage.UploadFileNoSync.  This test records how each of the three behaves when its cache
// entry is torn, so that the reason the durable default was kept for them is not lost: a torn snapshot file or
// fossil collection is an error the command reports and stops on, while the verified-chunk list is only a record of
// work already done and is rebuilt by verifying again.
//
// The tears are made by truncating the cached files directly rather than by crashing, so this does not depend on
// how the files were written; it documents the reading side, which is what the no-fsync decision was based on.
func TestCorruptNonChunkCacheEntries(t *testing.T) {

	setTestingT(t)

	testDir := path.Join(os.TempDir(), "duplicacy_test", "corrupt_nonchunk_cache_test")

	snapshotManager := createTestSnapshotManager(testDir)
	// A storage that needs a cache is the one whose snapshot files are cached and read back.
	snapshotManager.storage.(*FileStorage).isCacheNeeded = true

	chunkHash := uploadRandomChunk(snapshotManager, 1024)
	if chunkHash == "" {
		t.Errorf("Failed to upload a chunk")
		return
	}

	now := time.Now().Unix()
	createTestSnapshot(snapshotManager, "vm1@host1", 1, now-3600, now, []string{chunkHash}, "tag")

	cachedSnapshotPath := path.Join(snapshotManager.snapshotCache.storageDir, "snapshots", "vm1@host1", "1")
	if _, err := os.Stat(cachedSnapshotPath); err != nil {
		t.Errorf("The snapshot file was not added to the cache: %v", err)
		return
	}

	// Capture the log instead of letting an error fail the test through setTestingT.  Note that this also stops
	// LOG_ERROR from aborting the command the way it does in production, so a case that reports an error and then
	// returns continues to run here; that is what lets the test observe the failure and still clean up after it.
	savedLogFunction := LogFunction
	capture := &logCapture{}
	LogFunction = capture.log
	defer func() {
		LogFunction = savedLogFunction
	}()

	// A snapshot that is still readable must come back without an error.
	if snapshot := snapshotManager.DownloadSnapshot("vm1@host1", 1); snapshot == nil {
		t.Errorf("Failed to download an intact cached snapshot")
		return
	}
	if failures := capture.failures(); len(failures) > 0 {
		t.Errorf("Reading an intact cached snapshot failed: %v", failures)
		return
	}

	// Truncate the cached snapshot file to simulate a write torn by a crash.  Nothing verifies it, so the parse is
	// what catches it; the failure must be reported, not papered over.
	if err := os.Truncate(cachedSnapshotPath, 20); err != nil {
		t.Errorf("Failed to truncate the cached snapshot %s: %v", cachedSnapshotPath, err)
		return
	}

	if snapshot := snapshotManager.DownloadSnapshot("vm1@host1", 1); snapshot != nil {
		t.Errorf("A torn cached snapshot file was accepted instead of reported")
	}
	parseFailures := capture.messages("SNAPSHOT_PARSE")
	if len(parseFailures) == 0 {
		t.Errorf("A torn cached snapshot file did not produce a SNAPSHOT_PARSE error")
	}

	// The entry is unusable, so remove it to give the rest of the test a clean cache; a later download then falls
	// back to the storage, which is the recovery a user gets in practice.
	if err := os.Remove(cachedSnapshotPath); err != nil {
		t.Errorf("Failed to remove the torn cached snapshot %s: %v", cachedSnapshotPath, err)
		return
	}

	// The same for a fossil collection, which prune reads back and parses before it acts on it.
	collectionPath := path.Join(snapshotManager.snapshotCache.storageDir, "fossils", "1")
	if err := os.MkdirAll(path.Dir(collectionPath), 0700); err != nil {
		t.Errorf("Failed to create the fossil collection directory: %v", err)
		return
	}
	if err := ioutil.WriteFile(collectionPath, []byte(`{"last_revisions":{},"deleted_revisions":{}`), 0600); err != nil {
		t.Errorf("Failed to write a torn fossil collection: %v", err)
		return
	}

	// prune must fail rather than proceed on a collection it could not read.
	if success := snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{}, []string{}, []string{},
		false, false, []string{}, false, false, false, 1); success {
		t.Errorf("PruneSnapshots succeeded despite a torn fossil collection")
	}
	// Match on the message, not just the log id: FOSSIL_COLLECT is also used for the "Fossil collection N found"
	// info line, so an id-only check would pass even without the failure.
	collectionFailures := 0
	for _, message := range capture.messages("FOSSIL_COLLECT") {
		if strings.Contains(message, "Failed to load the fossil collection file") {
			collectionFailures++
		}
	}
	if collectionFailures == 0 {
		t.Errorf("A torn fossil collection did not report that it could not be loaded")
	}

	// The verified-chunk list is the third cached file that is read back unverified.  Unlike the other two it is
	// only a cache of work already done, so 'check -chunks' recovers from a torn copy by verifying the chunks
	// again rather than failing; that is what keeps it harmless to lose.
	verifiedChunksPath := path.Join(snapshotManager.snapshotCache.storageDir, "verified_chunks")
	if err := ioutil.WriteFile(verifiedChunksPath, []byte(`{"aaaaaaaa":1}`), 0600); err != nil {
		t.Errorf("Failed to write the verified chunks file: %v", err)
		return
	}
	if err := os.Truncate(verifiedChunksPath, 5); err != nil {
		t.Errorf("Failed to truncate the verified chunks file %s: %v", verifiedChunksPath, err)
		return
	}

	// checkFiles=false, checkChunks=true: the verified chunks file is only read when chunks are being checked.
	if !snapshotManager.CheckSnapshots("vm1@host1", []int{1}, "", false, false, false, true,
		false, false, false, 1, false) {
		t.Errorf("CheckSnapshots failed because of a torn verified chunks file")
	}
	// As above, match the message rather than the log id: SNAPSHOT_VERIFY also carries info lines.
	parseWarnings := 0
	for _, message := range capture.messages("SNAPSHOT_VERIFY") {
		if strings.Contains(message, "Failed to parse the file containing verified chunks") {
			parseWarnings++
		}
	}
	if parseWarnings == 0 {
		t.Errorf("A torn verified chunks file did not report that it could not be parsed")
	}
}

// A snapshot not seen by a fossil collection should always be consider a new snapshot in the fossil deletion step
func TestPruneNewSnapshots(t *testing.T) {
	setTestingT(t)

	testDir := path.Join(os.TempDir(), "duplicacy_test", "snapshot_test")

	snapshotManager := createTestSnapshotManager(testDir)

	chunkSize := 1024
	chunkHash1 := uploadRandomChunk(snapshotManager, chunkSize)
	chunkHash2 := uploadRandomChunk(snapshotManager, chunkSize)
	chunkHash3 := uploadRandomChunk(snapshotManager, chunkSize)
	chunkHash4 := uploadRandomChunk(snapshotManager, chunkSize)

	now := time.Now().Unix()
	day := int64(24 * 3600)
	t.Logf("Creating 3 snapshots")
	createTestSnapshot(snapshotManager, "vm1@host1", 1, now-3*day-3600, now-3*day-60, []string{chunkHash1, chunkHash2}, "tag")
	createTestSnapshot(snapshotManager, "vm1@host1", 2, now-2*day-3600, now-2*day-60, []string{chunkHash2, chunkHash3}, "tag")
	createTestSnapshot(snapshotManager, "vm2@host1", 1, now-2*day-3600, now-2*day-60, []string{chunkHash3, chunkHash4}, "tag")
	checkTestSnapshots(snapshotManager, 3, 0)

	t.Logf("Prune snapshot 1")
	// chunkHash1 should be marked as fossil
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{1}, []string{}, []string{}, false, false, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 2, 2)

	chunkHash5 := uploadRandomChunk(snapshotManager, chunkSize)
	// Create another snapshot of vm1 that brings back chunkHash1
	createTestSnapshot(snapshotManager, "vm1@host1", 3, now-0*day-3600, now-0*day-60, []string{chunkHash1, chunkHash3}, "tag")
	// Create another snapshot of vm2 so the fossil collection will be processed by next prune
	createTestSnapshot(snapshotManager, "vm2@host1", 2, now+3600, now+3600*2, []string{chunkHash4, chunkHash5}, "tag")

	// Now chunkHash1 wil be resurrected
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{}, []string{}, []string{}, false, false, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 4, 0)
	snapshotManager.CheckSnapshots("vm1@host1", []int{2, 3}, "", false, false, false, false, false, false, false, 1, false)
}

// A fossil collection left by an aborted prune should be ignored if any supposedly deleted snapshot exists
func TestPruneGhostSnapshots(t *testing.T) {
	setTestingT(t)

	EnableStackTrace()

	testDir := path.Join(os.TempDir(), "duplicacy_test", "snapshot_test")

	snapshotManager := createTestSnapshotManager(testDir)

	chunkSize := 1024
	chunkHash1 := uploadRandomChunk(snapshotManager, chunkSize)
	chunkHash2 := uploadRandomChunk(snapshotManager, chunkSize)
	chunkHash3 := uploadRandomChunk(snapshotManager, chunkSize)

	now := time.Now().Unix()
	day := int64(24 * 3600)
	t.Logf("Creating 2 snapshots")
	createTestSnapshot(snapshotManager, "vm1@host1", 1, now-3*day-3600, now-3*day-60, []string{chunkHash1, chunkHash2}, "tag")
	createTestSnapshot(snapshotManager, "vm1@host1", 2, now-2*day-3600, now-2*day-60, []string{chunkHash2, chunkHash3}, "tag")
	checkTestSnapshots(snapshotManager, 2, 0)

	snapshot1, err := ioutil.ReadFile(path.Join(testDir, "snapshots", "vm1@host1", "1"))
	if err != nil {
		t.Errorf("Failed to read snapshot file: %v", err)
	}

	t.Logf("Prune snapshot 1")
	// chunkHash1 should be marked as fossil
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{1}, []string{}, []string{}, false, false, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 1, 2)

	// Recover the snapshot file for revision 1; this is to simulate a scenario where prune may encounter a network error after
	// leaving the fossil collection but before deleting any snapshots.
	err = ioutil.WriteFile(path.Join(testDir, "snapshots", "vm1@host1", "1"), snapshot1, 0644)
	if err != nil {
		t.Errorf("Failed to write snapshot file: %v", err)
	}

	// Create another snapshot of vm1 so the fossil collection becomes eligible for processing.
	chunkHash4 := uploadRandomChunk(snapshotManager, chunkSize)
	createTestSnapshot(snapshotManager, "vm1@host1", 3, now-day-3600, now-day-60, []string{chunkHash3, chunkHash4}, "tag")

	// Run the prune again but the fossil collection should be igored, since revision 1 still exists
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{}, []string{}, []string{}, false, false, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 3, 2)
	snapshotManager.CheckSnapshots("vm1@host1", []int{1, 2, 3}, "", false, false, false, false, true /*searchFossils*/, false, false, 1, false)

	// Prune snapshot 1 again
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{1}, []string{}, []string{}, false, false, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 2, 2)

	// Create another snapshot
	chunkHash5 := uploadRandomChunk(snapshotManager, chunkSize)
	createTestSnapshot(snapshotManager, "vm1@host1", 4, now+3600, now+3600*2, []string{chunkHash5, chunkHash5}, "tag")
	checkTestSnapshots(snapshotManager, 3, 2)

	// Run the prune again and this time the fossil collection will be processed and the fossils removed
	snapshotManager.PruneSnapshots("vm1@host1", "vm1@host1", []int{}, []string{}, []string{}, false, false, []string{}, false, false, false, 1)
	checkTestSnapshots(snapshotManager, 3, 0)
	snapshotManager.CheckSnapshots("vm1@host1", []int{2, 3, 4}, "", false, false, false, false, false, false, false, 1, false)
}
