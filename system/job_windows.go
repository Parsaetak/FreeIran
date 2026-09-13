//go:build windows

package system

import (
	"errors"
	"sync"
	"syscall"
	"unsafe"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// jobHandle is the OS handle to a Windows job object. It is owned by
// one ManagedProcess, configured with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, and closed once the child has
// terminated (closing the handle is itself a kernel-level kill for
// any member that somehow survived).
//
// Win32 return-value protocol (CRITICAL — v0.7.0 regression):
//
// LazyProc.Call returns (r1, r2, err) where err is the raw thread
// GetLastError() value captured after the call. BOOL-returning APIs
// (SetInformationJobObject, AssignProcessToJobObject, CloseHandle)
// and HANDLE-returning APIs (CreateJobObjectW, OpenProcess) only
// guarantee a MEANINGFUL LastError when the call FAILS; on success
// they do not reset it, so err can hold a stale errno left behind by
// any earlier syscall in the launch path (CreateProcessW internals,
// pipe setup, handle operations). Checking err != 0 after a
// successful call therefore misreads success as failure — exactly
// what broke the Windows CI runners while desktop machines (whose
// stale value happened to be 0) kept passing.
//
// The rule enforced in this file: ALWAYS validate the actual return
// value (BOOL != 0, HANDLE != 0). Only when the call genuinely failed
// is the err (GetLastError) consulted — as a diagnostic, never as the
// success criterion.
type jobHandle struct {
	handle syscall.Handle
}

// Windows job-object / process API constants. Defined here so the
// intent of every magic number is documented at the point of use.
const (
	// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE: close the last job handle
	// → kernel terminates every remaining member process.
	jobObjectLimitKillOnJobClose = 0x00002000

	// JobObjectExtendedLimitInformation class for
	// SetInformationJobObject.
	jobObjectExtendedLimitInformation = 9

	// JobObjectBasicProcessIdList class for QueryInformationJobObject.
	jobObjectBasicProcessIDList = 3

	// Process access rights required by AssignProcessToJobObject per
	// MSDN: PROCESS_SET_QUOTA | PROCESS_TERMINATE.
	processSetQuota     = 0x0100
	processTerminate    = 0x0001
	processQueryLimited = 0x1000

	// Exit code passed to TerminateJobObject members.
	jobExitCode = 1

	errorAccessDenied = 5 // ERROR_ACCESS_DENIED

	// Windows NT 6.2 (Windows 8 / Server 2012) introduced nested job
	// objects; before that a process could live in only one job.
	windowsNestedJobBuild = 9200
)

var (
	kernel32                      = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObject           = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject   = kernel32.NewProc("SetInformationJobObject")
	procQueryInformationJobObject = kernel32.NewProc("QueryInformationJobObject")
	procAssignProcessToJobObject  = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject        = kernel32.NewProc("TerminateJobObject")
	procCloseHandle               = kernel32.NewProc("CloseHandle")
	procOpenProcess               = kernel32.NewProc("OpenProcess")
	procIsProcessInJob            = kernel32.NewProc("IsProcessInJob")
)

// JOBOBJECT_EXTENDED_LIMIT_INFORMATION layout for class 9. Only the
// BasicLimitInformation LimitFlags field is used; the rest is
// reserved and must be zero for the structure to stay valid.
type jobExtendedLimitInfo struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	PerProcessMemoryLimit   uintptr
	PerJobMemoryLimit       uintptr
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

// JOBOBJECT_BASIC_PROCESS_ID_LIST for class 3: the first member says
// how many of the following slots are in use.
type jobBasicProcessIDList struct {
	NumberOfAssignedProcesses uint32
	NumberOfProcessIdsInList  uint32
	ProcessIdList             [64]uintptr
}

// jobBindHook allows tests to inject a deterministic job-binding
// failure (simulating restricted environments where child-job
// assignment is unavailable) so the supervised-fallback lifecycle is
// exercised by CI on every platform. Nil in production.
var jobBindHook struct {
	mu sync.Mutex
	fn func() error
}

// setJobBindHook installs/clears the failure-injection hook. Tests
// must clear it (defer) before finishing.
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

// newJob creates a job object configured to kill every member
// process when its last handle closes. Assignment happens separately
// (see assign) so the launch strategy can retry with different
// process-creation flags.
func newJob() (*jobHandle, error) {
	raw, _, callErr := procCreateJobObject.Call(0, 0)
	if raw == 0 {
		// Genuine failure: GetLastError is meaningful on failure.
		return nil, firerrors.Wrap(callErr, firerrors.KindEnvironment,
			Subsystem, "job", "CreateJobObjectW")
	}

	handle := syscall.Handle(raw)

	// Configure the kill-on-close flag. SetInformationJobObject
	// returns a BOOL: nonzero = success. Its LastError is only
	// meaningful on failure (see the file header).
	var info jobExtendedLimitInfo
	info.LimitFlags = jobObjectLimitKillOnJobClose

	ok, _, err := procSetInformationJobObject.Call(
		uintptr(handle),
		uintptr(jobObjectExtendedLimitInformation),
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if ok == 0 {
		_ = closeHandle(handle)
		return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "job", "SetInformationJobObject(KILL_ON_JOB_CLOSE)")
	}

	return &jobHandle{handle: handle}, nil
}

// assign binds a process to the job. The process handle is opened
// with exactly the access rights AssignProcessToJobObject requires
// (PROCESS_SET_QUOTA | PROCESS_TERMINATE, per MSDN) plus
// PROCESS_QUERY_LIMITED_INFORMATION so the caller can verify the
// child is still alive before interpreting an assignment failure.
func (j *jobHandle) assign(pid int) error {
	if j == nil || j.handle == 0 {
		return firerrors.New(firerrors.KindEnvironment,
			Subsystem, "job", "job handle is not initialised")
	}

	procRaw, _, openErr := procOpenProcess.Call(
		uintptr(processSetQuota|processTerminate|processQueryLimited),
		0, // handle not inheritable
		uintptr(pid),
	)
	if procRaw == 0 {
		// Genuine failure (process gone / access denied):
		// GetLastError is meaningful on failure.
		return firerrors.Wrap(openErr, firerrors.KindEnvironment,
			Subsystem, "job", "OpenProcess(pid=%d)", pid)
	}

	procHandle := syscall.Handle(procRaw)
	defer closeHandle(procHandle)

	// Test hook: simulate restricted environments deterministically.
	if hookErr := jobBindHookErr(); hookErr != nil {
		return firerrors.Wrap(hookErr, firerrors.KindEnvironment,
			Subsystem, "job", "job binding disabled by test hook")
	}

	// AssignProcessToJobObject returns a BOOL: nonzero = success.
	// Windows 8+ supports NESTED jobs, so this succeeds even when the
	// parent (e.g. a GitHub Actions runner agent) already keeps our
	// process inside its own job — the child then lives in a job
	// hierarchy and our kill-on-close handle still governs it.
	assigned, _, err := procAssignProcessToJobObject.Call(
		uintptr(j.handle), uintptr(procHandle))
	if assigned == 0 {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "job", "AssignProcessToJobObject(pid=%d)", pid)
	}

	return nil
}

// isAssignRestricted reports whether an assign error is the
// restricted-environment signature (access denied): the classic
// pre-Windows-8 single-job limitation or a job hierarchy that does
// not permit nesting. This is the only case where relaunching the
// child with CREATE_BREAKAWAY_FROM_JOB can help. The check goes
// through errors.As so structured wrappers (and the test hook's
// injected errno) still resolve to the underlying Win32 error.
func isAssignRestricted(err error) bool {
	if err == nil {
		return false
	}

	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == syscall.Errno(errorAccessDenied)
	}

	return false
}

// terminate asks the kernel to terminate every process in the job
// (the child AND any grandchildren the core spawned). One call — no
// tree walk, no race with newly spawned descendants.
func (j *jobHandle) terminate() error {
	if j == nil || j.handle == 0 {
		return nil
	}

	ok, _, err := procTerminateJobObject.Call(uintptr(j.handle), uintptr(jobExitCode))
	if ok == 0 {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "job", "TerminateJobObject")
	}

	return nil
}

// processIDs lists the PIDs currently assigned to the job. It backs
// the "no member survived shutdown" invariant tests and runtime
// diagnostics. The second return is false when the query is
// unavailable (should not happen on supported Windows).
func (j *jobHandle) processIDs() ([]int, bool) {
	if j == nil || j.handle == 0 {
		return nil, false
	}

	var list jobBasicProcessIDList

	ok, _, _ := procQueryInformationJobObject.Call(
		uintptr(j.handle),
		uintptr(jobObjectBasicProcessIDList),
		uintptr(unsafe.Pointer(&list)),
		unsafe.Sizeof(list),
	)
	if ok == 0 {
		return nil, false
	}

	n := int(list.NumberOfProcessIdsInList)
	if n > len(list.ProcessIdList) {
		n = len(list.ProcessIdList)
	}

	ids := make([]int, n)
	for i := 0; i < n; i++ {
		ids[i] = int(list.ProcessIdList[i])
	}

	return ids, true
}

// parentInJob reports whether THIS process already lives inside a
// Windows job (GitHub Actions runners, CI sandboxes, some shells).
// Diagnostics only: assignment itself is attempted regardless, since
// Windows 8+ nests jobs transparently.
func parentInJob() bool {
	raw, _, _ := procOpenProcess.Call(
		uintptr(processQueryLimited), 0, uintptr(syscall.Getpid()))
	if raw == 0 {
		return false
	}

	defer closeHandle(syscall.Handle(raw))

	var inJob int32

	ok, _, _ := procIsProcessInJob.Call(uintptr(raw), 0, uintptr(unsafe.Pointer(&inJob)))

	return ok != 0 && inJob != 0
}

// close releases the job handle. The kernel kills every process
// still assigned to the job when the LAST handle closes
// (JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE). Safe to call repeatedly.
func (j *jobHandle) close() {
	if j == nil || j.handle == 0 {
		return
	}

	_ = closeHandle(j.handle)
	j.handle = 0
}

// closeHandle is CloseHandle: BOOL return, nonzero = success; the
// stale-LastError rule from the file header applies here too.
func closeHandle(handle syscall.Handle) error {
	ok, _, err := procCloseHandle.Call(uintptr(handle))
	if ok == 0 {
		return err
	}

	return nil
}
