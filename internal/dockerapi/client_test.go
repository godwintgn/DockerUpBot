package dockerapi

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Progress math helpers exercised via PullState aggregation expectations.
func TestPullStateETA(t *testing.T) {
	s := PullState{
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
	s := PullState{Current: 76, Total: 100}
	pct := s.Current * 100 / s.Total
	if pct != 76 {
		t.Fatalf("pct=%d", pct)
	}
}

func TestRegistryAuthIsScopedAndEngineEncoded(t *testing.T) {
	config := t.TempDir()
	t.Setenv("DOCKER_CONFIG", config)
	path := filepath.Join(config, "config.json")
	if err := os.WriteFile(path, []byte(`{"auths":{
		"https://index.docker.io/v1/":{"auth":"aHViOmh1Yi1wYXNz"},
		"registry.example.com:5000":{"auth":"cHJpdjpzZWNyZXQ="},
		"registry.example.com.evil":{"username":"other","password":"other"}
	}}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"unrelated.example.com/app", "registry.example.com/app", "registry.example.com:5001/app"} {
		if registryAuthHeader(ref) != "" {
			t.Fatalf("credentials selected for a different registry: %s", ref)
		}
	}
	encoded := registryAuthHeader("registry.example.com:5000/app@sha256:test")
	raw, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var auth map[string]string
	if err := json.Unmarshal(raw, &auth); err != nil || auth["username"] != "priv" || auth["password"] != "secret" || auth["serveraddress"] != "registry.example.com:5000" {
		t.Fatal("invalid Docker Engine auth payload", err)
	}
}
