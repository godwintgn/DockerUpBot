package diun_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/dockerupbot/dockerupbot/internal/diun"
)

type captureEngine struct {
	mu     sync.Mutex
	events []diun.Event
}

func (c *captureEngine) DiunEvent(e diun.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func TestWebhookAcceptsUpdate(t *testing.T) {
	eng := &captureEngine{}
	h := diun.NewHandler(eng, "secret")
	body := `{"status":"update","image":"nginx:latest","digest":"sha256:abc","metadata":{"ctn_names":"web"}}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/diun", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d", rr.Code)
	}
	if len(eng.events) != 1 || eng.events[0].Image != "nginx:latest" {
		t.Fatalf("events=%v", eng.events)
	}
}

func TestWebhookRejectsBadSecret(t *testing.T) {
	eng := &captureEngine{}
	h := diun.NewHandler(eng, "secret")
	body := `{"status":"update","image":"nginx:latest","digest":"sha256:abc","metadata":{"ctn_names":"web"}}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/diun", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "wrong")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestWebhookIgnoresNonUpdateStatus(t *testing.T) {
	eng := &captureEngine{}
	h := diun.NewHandler(eng, "secret")
	body := `{"status":"error","image":"nginx:latest","digest":"sha256:abc"}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/diun", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d", rr.Code)
	}
	if len(eng.events) != 0 {
		t.Fatalf("expected no events")
	}
}

func TestWebhookIgnoresTestNotif(t *testing.T) {
	eng := &captureEngine{}
	h := diun.NewHandler(eng, "secret")
	body := `{"status":"new","image":"docker.io/diun/testnotif:latest","digest":"sha256:abc"}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/diun", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d", rr.Code)
	}
	if len(eng.events) != 0 {
		t.Fatalf("expected no events for testnotif")
	}
}

func TestWebhookIgnoresMissingContainer(t *testing.T) {
	eng := &captureEngine{}
	h := diun.NewHandler(eng, "secret")
	body := `{"status":"update","image":"nginx:latest","digest":"sha256:abc"}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/diun", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if len(eng.events) != 0 {
		t.Fatalf("expected no events without ctn_names")
	}
}

func TestIsTestOrIncomplete(t *testing.T) {
	if !diun.IsTestOrIncomplete(diun.Event{Image: "docker.io/diun/testnotif:latest"}) {
		t.Fatal("testnotif")
	}
	if diun.IsTestOrIncomplete(diun.Event{Image: "nginx:latest", Metadata: map[string]string{"ctn_names": "web"}}) {
		t.Fatal("valid")
	}
}
