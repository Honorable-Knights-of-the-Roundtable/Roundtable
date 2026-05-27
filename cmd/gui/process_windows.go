//go:build windows

package main

import (
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

func prepareCmd(_ *exec.Cmd) {}

func postStart(cmd *exec.Cmd) (func(), error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return func() {}, err
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	_, err = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		windows.CloseHandle(job)
		return func() {}, err
	}

	handle, err := windows.OpenProcess(windows.PROCESS_ALL_ACCESS, false, uint32(cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return func() {}, err
	}

	if err := windows.AssignProcessToJobObject(job, handle); err != nil {
		windows.CloseHandle(handle)
		windows.CloseHandle(job)
		return func() {}, err
	}
	windows.CloseHandle(handle)

	return func() { windows.CloseHandle(job) }, nil
}
