package store

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/dockerupbot/dockerupbot/internal/diun"
)

// Status values for update records (spec §49).
const (
	StatusPending     = "pending"
	StatusApproved    = "approved"
	StatusQueued      = "queued"
	StatusPulling     = "pulling"
	StatusPulled      = "pulled"
	StatusUpdating    = "updating"
	StatusVerifying   = "verifying"
	StatusSuccess     = "success"
	StatusFailed      = "failed"
	StatusSkipped     = "skipped"
	StatusPaused      = "paused"
	StatusRollingBack = "rolling_back"
	StatusRolledBack  = "rolled_back"
)

type Record struct {
	ID             int64      `json:"id"`
	Image          string     `json:"image"`
	Digest         string     `json:"digest"`
	Status         string     `json:"status"`
	DetectedAt     time.Time  `json:"detected_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	Error          string     `json:"error,omitempty"`
	Project        string     `json:"project,omitempty"`
	Service        string     `json:"service,omitempty"`
	PreviousImage  string     `json:"previous_image,omitempty"`
	PreviousDigest string     `json:"previous_digest,omitempty"`
	NewImage       string     `json:"new_image,omitempty"`
	NewDigest      string     `json:"new_digest,omitempty"`
	ContainerName  string     `json:"container_name,omitempty"`
	LastLog        string     `json:"last_log,omitempty"`
}

type PendingItem struct {
	RecordID int64      `json:"record_id"`
	Event    diun.Event `json:"event"`
	Selected bool       `json:"selected,omitempty"`
}

type LatestMessage struct {
	ChatID    int64 `json:"chat_id"`
	MessageID int64 `json:"message_id"`
}

type State struct {
	Records     []Record       `json:"records"`
	Pending     []PendingItem  `json:"pending"`
	Latest      *LatestMessage `json:"latest,omitempty"`
	QueuePaused bool           `json:"queue_paused,omitempty"`
	PausedJobs  []PendingItem  `json:"paused_jobs,omitempty"`
	PausedIndex int            `json:"paused_index,omitempty"`
	SelectMode  bool           `json:"select_mode,omitempty"`
}

// Store is the persistence interface (spec §53).
type Store interface {
	Close() error
	Add(image, digest, status string) (int64, error)
	SetStatus(id int64, status, errText string) error
	SetRollbackMeta(id int64, project, service, prevImage, prevDigest, newImage, newDigest, containerName string) error
	AppendLog(id int64, logText string) error
	Get(id int64) (Record, bool)
	ListHistory(limit int) []Record
	GetPending() []PendingItem
	SetPending(items []PendingItem) error
	GetLatest() *LatestMessage
	SetLatest(chatID, messageID int64) error
	ClearLatest() error
	GetQueuePaused() (bool, []PendingItem, int)
	SetQueuePaused(paused bool, jobs []PendingItem, index int) error
	GetSelectMode() bool
	SetSelectMode(on bool) error
}

type JSONStore struct {
	path string
	mu   sync.Mutex
	data State
}

func Open(path string) (*JSONStore, error) {
	d := &JSONStore{path: path}
	if b, e := os.ReadFile(path); e == nil {
		if len(b) > 0 {
			if e = json.Unmarshal(b, &d.data); e != nil {
				// Backward-compat: older files were a bare []Record.
				var records []Record
				if e2 := json.Unmarshal(b, &records); e2 != nil {
					return nil, e
				}
				d.data.Records = records
			}
		}
	}
	return d, nil
}

func (d *JSONStore) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.persistLocked()
}

func (d *JSONStore) persistLocked() error {
	b, e := json.MarshalIndent(d.data, "", "  ")
	if e != nil {
		return e
	}
	tmp := d.path + ".tmp"
	if e = os.WriteFile(tmp, b, 0600); e != nil {
		return e
	}
	return os.Rename(tmp, d.path)
}

func (d *JSONStore) Add(image, digest, status string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var id int64
	for _, r := range d.data.Records {
		if r.ID > id {
			id = r.ID
		}
	}
	id++
	d.data.Records = append(d.data.Records, Record{
		ID:         id,
		Image:      image,
		Digest:     digest,
		Status:     status,
		DetectedAt: time.Now().UTC(),
		NewImage:   image,
		NewDigest:  digest,
	})
	return id, d.persistLocked()
}

func (d *JSONStore) SetStatus(id int64, status, errText string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range d.data.Records {
		if d.data.Records[i].ID == id {
			d.data.Records[i].Status = status
			d.data.Records[i].Error = errText
			if status == StatusSuccess || status == StatusFailed || status == StatusSkipped || status == StatusRolledBack {
				t := time.Now().UTC()
				d.data.Records[i].CompletedAt = &t
			}
			break
		}
	}
	return d.persistLocked()
}

func (d *JSONStore) SetRollbackMeta(id int64, project, service, prevImage, prevDigest, newImage, newDigest, containerName string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range d.data.Records {
		if d.data.Records[i].ID == id {
			d.data.Records[i].Project = project
			d.data.Records[i].Service = service
			d.data.Records[i].PreviousImage = prevImage
			d.data.Records[i].PreviousDigest = prevDigest
			d.data.Records[i].NewImage = newImage
			d.data.Records[i].NewDigest = newDigest
			d.data.Records[i].ContainerName = containerName
			break
		}
	}
	return d.persistLocked()
}

func (d *JSONStore) AppendLog(id int64, logText string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range d.data.Records {
		if d.data.Records[i].ID == id {
			if d.data.Records[i].LastLog != "" {
				d.data.Records[i].LastLog += "\n"
			}
			d.data.Records[i].LastLog += logText
			if len(d.data.Records[i].LastLog) > 4000 {
				d.data.Records[i].LastLog = d.data.Records[i].LastLog[len(d.data.Records[i].LastLog)-4000:]
			}
			break
		}
	}
	return d.persistLocked()
}

func (d *JSONStore) Get(id int64) (Record, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range d.data.Records {
		if r.ID == id {
			return r, true
		}
	}
	return Record{}, false
}

func (d *JSONStore) ListHistory(limit int) []Record {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := len(d.data.Records)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]Record, limit)
	copy(out, d.data.Records[n-limit:])
	return out
}

func (d *JSONStore) GetPending() []PendingItem {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]PendingItem, len(d.data.Pending))
	copy(out, d.data.Pending)
	return out
}

func (d *JSONStore) SetPending(items []PendingItem) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data.Pending = append([]PendingItem(nil), items...)
	return d.persistLocked()
}

func (d *JSONStore) GetLatest() *LatestMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.data.Latest == nil {
		return nil
	}
	cp := *d.data.Latest
	return &cp
}

func (d *JSONStore) SetLatest(chatID, messageID int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data.Latest = &LatestMessage{ChatID: chatID, MessageID: messageID}
	return d.persistLocked()
}

func (d *JSONStore) ClearLatest() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data.Latest = nil
	return d.persistLocked()
}

func (d *JSONStore) GetQueuePaused() (bool, []PendingItem, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	jobs := make([]PendingItem, len(d.data.PausedJobs))
	copy(jobs, d.data.PausedJobs)
	return d.data.QueuePaused, jobs, d.data.PausedIndex
}

func (d *JSONStore) SetQueuePaused(paused bool, jobs []PendingItem, index int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data.QueuePaused = paused
	d.data.PausedJobs = append([]PendingItem(nil), jobs...)
	d.data.PausedIndex = index
	return d.persistLocked()
}

func (d *JSONStore) GetSelectMode() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.data.SelectMode
}

func (d *JSONStore) SetSelectMode(on bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data.SelectMode = on
	return d.persistLocked()
}
