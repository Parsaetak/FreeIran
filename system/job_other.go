//go:build !windows

package system

// jobHandle is a no-op on non-Windows platforms where the process
// group already ensures child cleanup via SIGKILL propagation.
type jobHandle struct{}

func newKillOnCloseJob(pid int) (*jobHandle, error) {
	return &jobHandle{}, nil
}

func (j *jobHandle) close() {}
