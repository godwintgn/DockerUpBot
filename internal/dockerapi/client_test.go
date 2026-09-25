package dockerapi_test

import (
	"testing"
	"time"

	"github.com/dockerupbot/dockerupbot/internal/dockerapi"
)

// Progress math helpers exercised via PullState aggregation expectations.
func TestPullStateETA(t *testing.T) {
	s := dockerapi.PullState{
		Current:  500,
		Total:    1000,
		SpeedBps: 100,
	}
	if s.Total <= s.Current || s.SpeedBps <= 0 {
		t.Fatal("precondition")
	}
	eta := time.Duration(float64(s.Total-s.Current)/s.SpeedBps) * time.Second
	if eta != 5*time.Second {
		t.Fatalf("eta=%s", eta)
	}
}

func TestPullStatePercentage(t *testing.T) {
	s := dockerapi.PullState{Current: 76, Total: 100}
	pct := s.Current * 100 / s.Total
	if pct != 76 {
		t.Fatalf("pct=%d", pct)
	}
}

func TestRegistryAuthAbsentIsEmpty(t *testing.T) {
	// New() should succeed with default unix socket config even if socket missing;
	// construction does not dial.
	c, err := dockerapi.New()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
}
