package node

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the whole package if any goroutine outlives the tests.
// Stop() waits on its WaitGroup, but nothing verified that what those
// goroutines spawn (sessions, pingers, outbox tickers, discovery loops)
// actually came back down until this was added.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
