// queue/queue.go - Persistent queue (SQLite)
package queue

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	_ "modernc.org/sqlite"
)

// Episode represents an episode in the queue (simplified)
type Episode struct {
	ID   string
	Data []byte
}

// SyncQueue persistent synchronization queue
type SyncQueue struct {
	memoryQueue chan *Episode
	diskQueue   *sql.DB
	batchSize   int
	maxBytes    int64
	mu          sync.RWMutex
}

// Config queue configuration
type Config struct {
	MemoryQueueSize int
	BatchSize       int
	MaxBytes        int64
	DBPath          string
}

// New creates a new queue
func New(cfg *Config) (*SyncQueue, error) {
	// Open SQLite database with concurrent access configuration
	// _pragma=journal_mode(WAL): Use WAL mode for concurrent read/write
	// _timeout=5000: Set busy timeout
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=timeout(5000)", cfg.DBPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open queue database: %w", err)
	}

	// Verify connection
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	// Create table
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS sync_queue (
			id TEXT PRIMARY KEY,
			data BLOB NOT NULL,
			created_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_created_at ON sync_queue(created_at);
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to create queue table: %w", err)
	}

	q := &SyncQueue{
		memoryQueue: make(chan *Episode, cfg.MemoryQueueSize),
		diskQueue:   db,
		batchSize:   cfg.BatchSize,
		maxBytes:    cfg.MaxBytes,
	}

	log.Println("[QUEUE] Sync queue initialized")
	return q, nil
}

// Push adds to the queue
func (q *SyncQueue) Push(ep *Episode) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	select {
	case q.memoryQueue <- ep:
		return nil
	default:
		// Memory queue full, write to disk
		_, err := q.diskQueue.Exec(
			"INSERT INTO sync_queue (id, data, created_at) VALUES (?, ?, ?)",
			ep.ID, ep.Data, 0,
		)
		if err != nil {
			return fmt.Errorf("failed to write to disk queue: %w", err)
		}
		log.Printf("[QUEUE] Episode %s queued to disk", ep.ID)
		return nil
	}
}

// Pop pops from the queue
func (q *SyncQueue) Pop() (*Episode, error) {
	select {
	case ep := <-q.memoryQueue:
		return ep, nil
	default:
		// Memory queue empty, try reading from disk
		var id string
		var data []byte
		err := q.diskQueue.QueryRow("SELECT id, data FROM sync_queue ORDER BY created_at LIMIT 1").Scan(&id, &data)
		if err == sql.ErrNoRows {
			return nil, nil // Queue empty
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read from disk queue: %w", err)
		}

		// Delete read record
		_, err = q.diskQueue.Exec("DELETE FROM sync_queue WHERE id = ?", id)
		if err != nil {
			log.Printf("[QUEUE] Warning: failed to delete queued item: %v", err)
		}

		return &Episode{ID: id, Data: data}, nil
	}
}

// Size returns queue size (memory + disk)
func (q *SyncQueue) Size() (int, error) {
	memorySize := len(q.memoryQueue)

	var diskSize int
	err := q.diskQueue.QueryRow("SELECT COUNT(*) FROM sync_queue").Scan(&diskSize)
	if err != nil {
		return 0, fmt.Errorf("failed to count disk queue: %w", err)
	}

	return memorySize + diskSize, nil
}

// Close closes the queue
func (q *SyncQueue) Close() error {
	log.Println("[QUEUE] Closing sync queue")
	return q.diskQueue.Close()
}

// ToJSON converts Episode to JSON
func (ep *Episode) ToJSON(v interface{}) error {
	return json.Unmarshal(ep.Data, v)
}

// FromJSON creates Episode from JSON
func FromJSON(id string, v interface{}) (*Episode, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &Episode{ID: id, Data: data}, nil
}
