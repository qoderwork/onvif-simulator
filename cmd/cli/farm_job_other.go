//go:build !windows

package main

// farmJob is a no-op on platforms without Windows job objects. Child cleanup
// there relies on farm's graceful shutdown and on `farm stop` reading the
// manifest after a crash.
type farmJob struct{}

// newFarmJob returns a job whose methods are all no-ops. It still returns a
// non-nil value so callers never have to special-case "no job" — the Windows
// counterpart returns nil only when creation actually failed.
func newFarmJob() (*farmJob, error) { return &farmJob{}, nil }

// assign is a no-op on non-Windows platforms.
func (*farmJob) assign(int) error { return nil }

// Close is a no-op on non-Windows platforms.
func (*farmJob) Close() {}
