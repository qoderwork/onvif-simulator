//go:build !windows

package main

// farmJob is a no-op on platforms without Windows job objects. Child cleanup
// there relies on farm's graceful shutdown and on `farm stop` reading the
// manifest after a crash.
type farmJob struct{}

// newFarmJob returns a nil job; every method tolerates a nil receiver.
func newFarmJob() (*farmJob, error) { return nil, nil }

// assign is a no-op on non-Windows platforms.
func (*farmJob) assign(int) error { return nil }

// Close is a no-op on non-Windows platforms.
func (*farmJob) Close() {}
