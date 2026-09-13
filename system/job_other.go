//go:build !windows

package system

import (
	"sync"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// jobHandle is the non-Windows stand-in. Unix-like platforms do not
// have job objects: the equivalent kernel guarantee — "no protocol
// core outlives its supervisor" — is provided by the process GROUP
// (the child is spawned as a group leader via setpgid, and Stop
// signals the whole group, then SIGKILLs it). The handle therefore
// carries no OS resource, but it preserves the same supervision API
// so the launch strategy, lifecycle state machine, tests and
// diagnostics stay platform-uniform.
type jobHandle struct{}

// jobBindHook mirrors the Windows failure-injection hook so the
// supervised-fallback lifecycle (degraded binding + deterministic
// cleanup) is exercised by CI on every platform.
var jobBindHook struct {
	mu sync.Mutex
	fn func() error
}

// setJobBindHook installs/clears the failure-injection hook.
func setJobBindHook(fn func() error) {
	jobBindHook.mu.Lock()
	jobBindHook.fn = fn
	jobBindHook.mu.Unlock()
}

// jobBindHookErr returns the currently injected error, if any.
func jobBindHookErr() error {
	jobBindHook.mu.Lock()
	defer jobBindHook.mu.Unlock()

	if jobBindHook.fn == nil {
		return nil
	}

	return jobBindHook.fn()
}

// newJob creates the placeholder. It never fails on Unix.
func newJob() (*jobHandle, error) {
	return &jobHandle{}, nil
}

// assign marks the binding as active unless the test hook simulates
// a restricted environment.
func (j *jobHandle) assign(pid int) error {
	if hookErr := jobBindHookErr(); hookErr != nil {
		return firerrors.Wrap(hookErr, firerrors.KindEnvironment,
			Subsystem, "job", "job binding disabled by test hook")
	}

	return nil
}

// isAssignRestricted reports whether an assign error is the
// restricted-environment signature. On Unix this cannot happen
// outside the test hook, so the answer is always false.
func isAssignRestricted(err error) bool {
	return false
}

// terminate is a no-op: the process-group kill in terminateProcess
// performs the actual teardown.
func (j *jobHandle) terminate() error {
	return nil
}

// processIDs reports no members: Unix supervision is group-based, and
// the group-alive assertion in the lifecycle tests uses
// kill(-pgid, 0) instead.
func (j *jobHandle) processIDs() ([]int, bool) {
	return nil, false
}

// parentInJob reports whether this process lives inside a job-style
// sandbox. Not applicable on Unix.
func parentInJob() bool {
	return false
}

// close is a no-op.
func (j *jobHandle) close() {}
