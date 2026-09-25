package version_test

import (
	"strings"
	"testing"

	"github.com/dockerupbot/dockerupbot/internal/version"
)

func TestStringContainsVersion(t *testing.T) {
	s := version.String()
	if !strings.Contains(s, version.Short()) {
		t.Fatalf("%q", s)
	}
}
