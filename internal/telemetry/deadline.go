package telemetry

import "time"

func receiveByDeadline[T any](values <-chan T, deadline time.Time) (T, bool) {
	select {
	case value := <-values:
		return value, true
	default:
	}

	var zero T
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return zero, false
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case value := <-values:
		return value, true
	case <-timer.C:
		return zero, false
	}
}
