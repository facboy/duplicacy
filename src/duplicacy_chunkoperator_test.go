// Copyright (c) Acrosync LLC. All rights reserved.
// Free for personal use and commercial trial
// Commercial use requires per-user licenses available from https://duplicacy.com

package duplicacy

import (
	"os"
	"path"
	"runtime/debug"
	"sync"
	"testing"
	"time"

	crypto_rand "crypto/rand"
	"math/rand"
)

// blockedDownloadStorage holds every chunk download until the test releases it, so that a test can decide exactly when
// the last outstanding task finishes.
type blockedDownloadStorage struct {
	*FileStorage
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (storage *blockedDownloadStorage) DownloadFile(threadIndex int, filePath string, chunk *Chunk) (err error) {
	storage.once.Do(func() { close(storage.entered) })
	<-storage.release
	return storage.FileStorage.DownloadFile(threadIndex, filePath, chunk)
}

// WaitForCompletion has to be woken by the task that finishes last.  It used to poll the task counter every
// 100 ms, so a command whose chunk work takes a few milliseconds -- the whole run on a local storage --
// spent most of the poll interval waiting for nothing.  The download here is held until WaitForCompletion
// is already waiting on it, so the elapsed time is one poll interval on a polling implementation and the
// release itself on a signalled one.
func TestWaitForCompletionIsWokenNotPolled(t *testing.T) {

	setTestingT(t)

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%v", r)
		}
	}()

	testDir := path.Join(os.TempDir(), "duplicacy_test", "chunk_operator_test")
	os.RemoveAll(testDir)
	os.MkdirAll(testDir, 0700)

	storage, err := CreateFileStorage(testDir, false, 1)
	if err != nil {
		t.Errorf("Failed to create the storage: %v", err)
		return
	}

	config := CreateConfig()

	content := make([]byte, 4096)
	for i := range content {
		content[i] = byte(i)
	}

	chunk := CreateChunk(config, true)
	chunk.Reset(true)
	chunk.Write(content)
	chunkHash := chunk.GetHash()

	// Upload the chunk with a plain operator, so that there is something for the operator below to download.
	uploader := CreateChunkOperator(config, storage, nil, false, false, 1, false)
	uploader.UploadCompletionFunc = func(chunk *Chunk, chunkIndex int, inCache bool, chunkSize int, uploadSize int) {}
	uploader.Upload(chunk, 0, false)
	uploader.WaitForCompletion()
	uploader.Stop()

	blocked := &blockedDownloadStorage{
		FileStorage: storage,
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
	}

	// No snapshot cache, so that the download goes to the storage and reaches the blocked DownloadFile.
	operator := CreateChunkOperator(config, blocked, nil, false, false, 1, false)
	defer operator.Stop()

	operator.DownloadAsync(chunkHash, 0, false, func(downloaded *Chunk, chunkIndex int) {
		config.PutChunk(downloaded)
	})

	<-blocked.entered

	start := time.Now()
	finished := make(chan struct{})
	go func() {
		operator.WaitForCompletion()
		close(finished)
	}()

	// The download is still held here, so WaitForCompletion is waiting on it and the release is the only thing that can
	// wake it up.  This sleep is well under the 100 ms poll interval that this test is against.
	time.Sleep(20 * time.Millisecond)
	close(blocked.release)
	<-finished

	if elapsed := time.Since(start); elapsed >= 60*time.Millisecond {
		t.Errorf("WaitForCompletion took %v after the last task finished; it is waiting on a timer, not on the task",
			elapsed)
	}
}

func TestChunkOperator(t *testing.T) {

	rand.Seed(time.Now().UnixNano())
	setTestingT(t)
	SetLoggingLevel(DEBUG)

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

	testDir := path.Join(os.TempDir(), "duplicacy_test", "storage_test")
	os.RemoveAll(testDir)
	os.MkdirAll(testDir, 0700)

	t.Logf("storage: %s", *testStorageName)

	storage, err := loadStorage(testDir, 1)
	if err != nil {
		t.Errorf("Failed to create storage: %v", err)
		return
	}
	storage.EnableTestMode()
	storage.SetRateLimits(*testRateLimit, *testRateLimit)

	for _, dir := range []string{"chunks", "snapshots"} {
		err = storage.CreateDirectory(0, dir)
		if err != nil {
			t.Errorf("Failed to create directory %s: %v", dir, err)
			return
		}
	}

	numberOfChunks := 100
	maxChunkSize := 64 * 1024

	if *testQuickMode {
		numberOfChunks = 10
	}

	var chunks []*Chunk

	config := CreateConfig()
	config.MinimumChunkSize = 100
	config.chunkPool = make(chan *Chunk, numberOfChunks*2)
	totalFileSize := 0

	for i := 0; i < numberOfChunks; i++ {
		content := make([]byte, rand.Int()%maxChunkSize+1)
		_, err = crypto_rand.Read(content)
		if err != nil {
			t.Errorf("Error generating random content: %v", err)
			return
		}

		chunk := CreateChunk(config, true)
		chunk.Reset(true)
		chunk.Write(content)
		chunks = append(chunks, chunk)

		t.Logf("Chunk: %s, size: %d", chunk.GetID(), chunk.GetLength())
		totalFileSize += chunk.GetLength()
	}

	chunkOperator := CreateChunkOperator(config, storage, nil, false, false, *testThreads, false)
	chunkOperator.UploadCompletionFunc = func(chunk *Chunk, chunkIndex int, skipped bool, chunkSize int, uploadSize int) {
		t.Logf("Chunk %s size %d (%d/%d) uploaded", chunk.GetID(), chunkSize, chunkIndex, len(chunks))
	}

	for i, chunk := range chunks {
		chunkOperator.Upload(chunk, i, false)
	}

	chunkOperator.WaitForCompletion()

	for i, chunk := range chunks {
		downloaded := chunkOperator.Download(chunk.GetHash(), i, false)
		if downloaded.GetID() != chunk.GetID() {
			t.Errorf("Uploaded: %s, downloaded: %s", chunk.GetID(), downloaded.GetID())
		}
	}

	chunkOperator.Stop()

	for _, file := range listChunks(storage) {
		err = storage.DeleteFile(0, "chunks/"+file)
		if err != nil {
			t.Errorf("Failed to delete the file %s: %v", file, err)
			return
		}
	}

}
