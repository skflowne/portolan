package telemetry

import (
	"testing"
	"time"
)

func TestReceiveByDeadlinePrefersCompletedChannelAfterDeadline(t *testing.T) {
	done := make(chan struct{})
	close(done)

	if _, ok := receiveByDeadline(done, time.Now().Add(-time.Second)); !ok {
		t.Fatal("completed channel reported deadline expiry")
	}
}

func TestReceiveByDeadlineRejectsOpenChannelAfterDeadline(t *testing.T) {
	done := make(chan struct{})

	if _, ok := receiveByDeadline(done, time.Now().Add(-time.Second)); ok {
		t.Fatal("open channel reported completion")
	}
}
