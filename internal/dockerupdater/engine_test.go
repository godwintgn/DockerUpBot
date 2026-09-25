package dockerupdater_test

import (
	"testing"

	"github.com/dockerupbot/dockerupbot/internal/dockerupdater"
)

func TestEngineTypeExported(t *testing.T) {
	_ = dockerupdater.Engine{}
}
