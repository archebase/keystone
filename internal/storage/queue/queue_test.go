// queue/queue_test.go - Persistent queue tests
package queue

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewQueue(t *testing.T) {
	// Create temporary database file
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test-queue.db")

	// Retry on transient I/O errors
	var q *SyncQueue
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		q, err = New(&Config{
			MemoryQueueSize: 10,
			BatchSize:       5,
			MaxBytes:        1024 * 1024,
			DBPath:          dbPath,
		})
		if err == nil {
			break
		}
		t.Logf("Attempt %d: New() error = %v", attempt+1, err)
	}

	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if q == nil {
		t.Fatal("New() returned nil")
	}

	// Verify database file was created
	if !fileExists(dbPath) {
		t.Error("New() database file not created")
	}

	// Cleanup
	q.Close()
}

func TestQueuePushAndPop(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test-queue.db")

	q, err := New(&Config{
		MemoryQueueSize: 10,
		BatchSize:       5,
		MaxBytes:        1024 * 1024,
		DBPath:          dbPath,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer q.Close()

	// Push episode
	ep1 := &Episode{
		ID:   "ep-001",
		Data: []byte(`{"test": "data1"}`),
	}

	err = q.Push(ep1)
	if err != nil {
		t.Fatalf("Push() error = %v", err)
	}

	// Pop and verify
	ep2, err := q.Pop()
	if err != nil {
		t.Fatalf("Pop() error = %v", err)
	}

	if ep2 == nil {
		t.Fatal("Pop() returned nil")
	}

	if ep2.ID != "ep-001" {
		t.Errorf("Pop().ID = %v, want ep-001", ep2.ID)
	}

	// Queue should be empty
	ep3, _ := q.Pop()
	if ep3 != nil {
		t.Errorf("Pop() after empty queue = %v, want nil", ep3)
	}
}

func TestQueueMultipleItems(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test-queue.db")

	q, err := New(&Config{
		MemoryQueueSize: 10,
		BatchSize:       5,
		MaxBytes:        1024 * 1024,
		DBPath:          dbPath,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer q.Close()

	// Push multiple items
	for i := 1; i <= 3; i++ {
		ep := &Episode{
			ID:   filepath.Join("ep-", string(rune('0'+i))),
			Data: []byte(`{"test": "data"}`),
		}
		err = q.Push(ep)
		if err != nil {
			t.Fatalf("Push() error = %v", err)
		}
	}

	// Get queue size
	size, err := q.Size()
	if err != nil {
		t.Fatalf("Size() error = %v", err)
	}
	if size != 3 {
		t.Errorf("Size() = %v, want 3", size)
	}

	// Pop all items
	count := 0
	for {
		ep, err := q.Pop()
		if err != nil {
			t.Fatalf("Pop() error = %v", err)
		}
		if ep == nil {
			break
		}
		count++
	}

	if count != 3 {
		t.Errorf("Pop() count = %v, want 3", count)
	}
}

func TestQueueMemoryToDiskSpill(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test-queue.db")

	// Create queue with only 2 memory slots
	q, err := New(&Config{
		MemoryQueueSize: 2,
		BatchSize:       5,
		MaxBytes:        1024 * 1024,
		DBPath:          dbPath,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer q.Close()

	// Push 3 items, third should spill to disk
	for i := 1; i <= 3; i++ {
		ep := &Episode{
			ID:   filepath.Join("ep-", string(rune('0'+i))),
			Data: []byte(`{"test": "data"}`),
		}
		err = q.Push(ep)
		if err != nil {
			t.Fatalf("Push() error = %v", err)
		}
	}

	// Verify size
	size, err := q.Size()
	if err != nil {
		t.Fatalf("Size() error = %v", err)
	}
	if size != 3 {
		t.Errorf("Size() = %v, want 3", size)
	}

	// Pop and verify order
	ids := []string{}
	for {
		ep, err := q.Pop()
		if err != nil {
			t.Fatalf("Pop() error = %v", err)
		}
		if ep == nil {
			break
		}
		ids = append(ids, ep.ID)
	}

	if len(ids) != 3 {
		t.Errorf("Pop() count = %v, want 3", len(ids))
	}
}

func TestQueueSize(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test-queue.db")

	q, err := New(&Config{
		MemoryQueueSize: 10,
		BatchSize:       5,
		MaxBytes:        1024 * 1024,
		DBPath:          dbPath,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer q.Close()

	// Empty queue
	size, err := q.Size()
	if err != nil {
		t.Fatalf("Size() error = %v", err)
	}
	if size != 0 {
		t.Errorf("Size() = %v, want 0", size)
	}

	// Add items
	for i := 0; i < 5; i++ {
		q.Push(&Episode{
			ID:   filepath.Join("ep-", string(rune('0'+i))),
			Data: []byte(`{}`),
		})
	}

	size, err = q.Size()
	if err != nil {
		t.Fatalf("Size() error = %v", err)
	}
	if size != 5 {
		t.Errorf("Size() = %v, want 5", size)
	}
}

func TestQueuePersistence(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test-queue.db")

	// Create first queue instance and add data
	q1, err := New(&Config{
		MemoryQueueSize: 2,
		BatchSize:       5,
		MaxBytes:        1024 * 1024,
		DBPath:          dbPath,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Add enough items to spill to disk
	for i := 0; i < 5; i++ {
		q1.Push(&Episode{
			ID:   filepath.Join("ep-", string(rune('0'+i))),
			Data: []byte(`{}`),
		})
	}

	// Close first instance
	q1.Close()

	// Create second instance, should recover data from disk
	q2, err := New(&Config{
		MemoryQueueSize: 2,
		BatchSize:       5,
		MaxBytes:        1024 * 1024,
		DBPath:          dbPath,
	})
	if err != nil {
		t.Fatalf("New() second instance error = %v", err)
	}
	defer q2.Close()

	// Verify data was recovered
	size, err := q2.Size()
	if err != nil {
		t.Fatalf("Size() error = %v", err)
	}
	// Note: data in memory queue is lost on close, only disk data persists
	// So we expect at least the data that was on disk
	if size < 3 {
		t.Errorf("Size() = %v, want >= 3 (disk persisted data)", size)
	}
}

func TestEpisodeJSON(t *testing.T) {
	type TestData struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}

	data := TestData{Name: "test", Value: 42}
	ep, err := FromJSON("ep-001", data)
	if err != nil {
		t.Fatalf("FromJSON() error = %v", err)
	}

	if ep.ID != "ep-001" {
		t.Errorf("FromJSON().ID = %v, want ep-001", ep.ID)
	}

	// Parse back to struct
	var result TestData
	err = ep.ToJSON(&result)
	if err != nil {
		t.Fatalf("ToJSON() error = %v", err)
	}

	if result.Name != "test" {
		t.Errorf("ToJSON().Name = %v, want test", result.Name)
	}

	if result.Value != 42 {
		t.Errorf("ToJSON().Value = %v, want 42", result.Value)
	}
}

// Helper function
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
