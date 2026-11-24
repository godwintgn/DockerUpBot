package store

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
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
	Records      []Record        `json:"records"`
	Pending      []PendingItem   `json:"pending"`
	Latest       *LatestMessage  `json:"latest,omitempty"`
	LatestByChat map[int64]int64 `json:"latest_by_chat,omitempty"`
	QueuePaused  bool            `json:"queue_paused,omitempty"`
	PausedJobs   []PendingItem   `json:"paused_jobs,omitempty"`
	PausedIndex  int             `json:"paused_index,omitempty"`
	SelectMode   bool            `json:"select_mode,omitempty"`
}

// Store is the persistence interface (spec §53).
type Store interface {
	SaveEvent(event diun.Event, replaceID int64) ([]PendingItem, error)
	SaveQueue(pending, paused []PendingItem, index int, selectMode bool, ids []int64, status string) error
	SetStatus(id int64, status, errText string) error
	SetRollbackMeta(id int64, project, service, prevImage, prevDigest, newImage, newDigest, containerName string) error
	AppendLog(id int64, logText string) error
	Get(id int64) (Record, bool)
	ListHistory(limit int) []Record
	GetPending() []PendingItem
	LatestMessages() map[int64]int64
	SetLatest(chatID, messageID int64) error
	GetQueuePaused() (bool, []PendingItem, int)
	GetSelectMode() bool
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
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	if d.data.LatestByChat == nil {
		d.data.LatestByChat = make(map[int64]int64)
		if latest := d.data.Latest; latest != nil {
			d.data.LatestByChat[latest.ChatID] = latest.MessageID
		}
	}
	d.data.Latest = nil
	return d, nil
}

// Flush writes the current state and checks that its directory supports atomic replacement.
func (d *JSONStore) Flush() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.persistLocked()
}

func (d *JSONStore) persistLocked() error {
	b, e := json.MarshalIndent(d.data, "", "  ")
	if e != nil {
		return e
	}
	dir, err := os.Open(filepath.Dir(d.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	tmp, err := os.CreateTemp(filepath.Dir(d.path), ".dockerupbot-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), d.path); err != nil {
		return err
	}
	// The rename already committed. Returning an error here would revert memory only.
	if err := dir.Sync(); err != nil {
		slog.Error("sync state directory after rename", "err", err)
	}
	return nil
}

func (d *JSONStore) Add(image, digest, status string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := d.addLocked(image, digest, status)
	if err := d.persistLocked(); err != nil {
		d.data.Records = d.data.Records[:len(d.data.Records)-1]
		return 0, err
	}
	return id, nil
}

func (d *JSONStore) addLocked(image, digest, status string) int64 {
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
	return id
}

func (d *JSONStore) SaveEvent(event diun.Event, replaceID int64) ([]PendingItem, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	before := d.data
	d.data.Records = append([]Record(nil), before.Records...)
	d.data.Pending = append([]PendingItem(nil), before.Pending...)
	id := d.addLocked(event.Image, event.Digest, StatusPending)
	item := PendingItem{RecordID: id, Event: event, Selected: !d.data.SelectMode}
	for i, pending := range d.data.Pending {
		if pending.RecordID == replaceID {
			item.Selected = pending.Selected
			d.data.Pending[i] = item
			break
		}
	}
	if replaceID == 0 {
		d.data.Pending = append(d.data.Pending, item)
	}
	// A paused job still refers to the digest originally approved.
	for _, job := range d.data.PausedJobs {
		if job.RecordID == replaceID {
			replaceID = 0
			break
		}
	}
	if replaceID != 0 {
		d.setStatusLocked(replaceID, StatusSkipped, "superseded by a new digest")
	}
	if err := d.persistLocked(); err != nil {
		d.data = before
		return nil, err
	}
	return append([]PendingItem(nil), d.data.Pending...), nil
}

func (d *JSONStore) SaveQueue(pending, paused []PendingItem, index int, selectMode bool, ids []int64, status string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	before := d.data
	d.data.Records = append([]Record(nil), before.Records...)
	d.data.Pending = append([]PendingItem(nil), pending...)
	d.data.PausedJobs = append([]PendingItem(nil), paused...)
	d.data.QueuePaused = len(paused) > 0
	d.data.PausedIndex = index
	d.data.SelectMode = selectMode
	for _, id := range ids {
		d.setStatusLocked(id, status, "")
	}
	if err := d.persistLocked(); err != nil {
		d.data = before
		return err
	}
	return nil
}

func (d *JSONStore) SetStatus(id int64, status, errText string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.setStatusLocked(id, status, errText)
	return d.persistLocked()
}

func (d *JSONStore) setStatusLocked(id int64, status, errText string) {
	for i := range d.data.Records {
		if d.data.Records[i].ID == id {
			d.data.Records[i].Status = status
			d.data.Records[i].Error = errText
			if status == StatusSuccess || status == StatusFailed || status == StatusSkipped || status == StatusRolledBack {
				t := time.Now().UTC()
				d.data.Records[i].CompletedAt = &t
			} else {
				d.data.Records[i].CompletedAt = nil
			}
			break
		}
	}
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

func (d *JSONStore) LatestMessages() map[int64]int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[int64]int64, len(d.data.LatestByChat))
	for chat, msg := range d.data.LatestByChat {
		out[chat] = msg
	}
	return out
}

func (d *JSONStore) SetLatest(chatID, messageID int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data.LatestByChat[chatID] = messageID
	return d.persistLocked()
}

func (d *JSONStore) GetQueuePaused() (bool, []PendingItem, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	jobs := make([]PendingItem, len(d.data.PausedJobs))
	copy(jobs, d.data.PausedJobs)
	return d.data.QueuePaused, jobs, d.data.PausedIndex
}

func (d *JSONStore) GetSelectMode() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.data.SelectMode
}
