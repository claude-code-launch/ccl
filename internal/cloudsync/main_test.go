package cloudsync

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Builds carry no Google client unless release ldflags inject one.
	googleOAuthClientID = "test-client.apps.googleusercontent.com"
	googleOAuthClientSecret = "test-secret"
	os.Exit(m.Run())
}
