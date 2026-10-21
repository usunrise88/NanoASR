package managed

import (
	"os"
	"testing"
	"time"
)

func testBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no path to the test binary:", err)
	}
	return exe
}

// TestHelperWaits is the child process of TestWatchParentNoticesTheExit.
func TestHelperWaits(t *testing.T) {
	if os.Getenv("NANOASR_TEST_HELPER") != "1" {
		t.Skip("a helper process, not a test")
	}
	time.Sleep(time.Minute)
}
