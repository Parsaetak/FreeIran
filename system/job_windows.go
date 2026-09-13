//go:build windows

package system

import (
	"syscall"
	"unsafe"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// jobHandle is the OS handle to a Windows job object. It is owned by
// one ManagedProcess and closed once the child has terminated.
type jobHandle struct {
	handle syscall.Handle
}

// Windows job-object / process API constants. Defined here so the
// intent of every magic number is documented at the point of use.
const (
	jobObjectLimitKillOnJobClose = 0x00002000

	statusStillActive = 0x00000103
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObject          = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procCloseHandle              = kernel32.NewProc("CloseHandle")
	procOpenProcess              = kernel32.NewProc("OpenProcess")
	procGetExitCodeProcess       = kernel32.NewProc("GetExitCodeProcess")
)

// JOBOBJECT_EXTENDED_LIMIT_INFORMATION layout used by
// JobObjectExtendedLimitInformation. Only the BasicLimitInformation
// member and the LimitFlags field are used; the rest is reserved.
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

type jobBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

// newKillOnCloseJob creates a job object configured to kill every
// assigned process when the job handle is closed (which happens when
// FreeIran exits for any reason). The pid is assigned to the job so
// the kernel tracks it.
func newKillOnCloseJob(pid int) (*jobHandle, error) {
	raw, _, err := procCreateJobObject.Call(0, 0)
	if raw == 0 {
		return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "job", "CreateJobObjectW")
	}

	handle := syscall.Handle(raw)

	// Configure the kill-on-close flag.
	var info jobExtendedLimitInfo
	info.LimitFlags = jobObjectLimitKillOnJobClose

	_, _, err = procSetInformationJobObject.Call(
		uintptr(handle),
		uintptr(9), // JobObjectExtendedLimitInformation
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if err != nil && err != syscall.Errno(0) {
		_ = closeHandle(handle)
		return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "job", "SetInformationJobObject")
	}

	// Assign the child process to the job.
	procRaw, _, openErr := procOpenProcess.Call(
		uintptr(0x0400|0x0100|0x0001), // PROCESS_SET_QUOTA | PROCESS_TERMINATE | PROCESS_TERMINATE synonym
		1,
		uintptr(pid),
	)
	if procRaw == 0 {
		_ = closeHandle(handle)
		return nil, firerrors.Wrap(openErr, firerrors.KindEnvironment,
			Subsystem, "job", "OpenProcess(pid=%d)", pid)
	}

	procHandle := syscall.Handle(procRaw)

	assigned, _, assignErr := procAssignProcessToJobObject.Call(uintptr(handle), uintptr(procHandle))
	if assigned == 0 {
		_ = closeHandle(procHandle)
		_ = closeHandle(handle)
		return nil, firerrors.Wrap(assignErr, firerrors.KindEnvironment,
			Subsystem, "job", "AssignProcessToJobObject(pid=%d)", pid)
	}

	_ = closeHandle(procHandle)

	return &jobHandle{handle: handle}, nil
}

// close releases the job handle. The kernel kills every process still
// assigned to the job if JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE was set.
func (j *jobHandle) close() {
	if j == nil || j.handle == 0 {
		return
	}

	_ = closeHandle(j.handle)
	j.handle = 0
}

func closeHandle(handle syscall.Handle) error {
	r, _, err := procCloseHandle.Call(uintptr(handle))
	if r == 0 {
		return err
	}
	return nil
}
