package sync_test

import (
	"syscall"
	"unsafe"
)

// processMemoryCounters is PROCESS_MEMORY_COUNTERS of psapi.h.
type processMemoryCounters struct {
	cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

var procGetProcessMemoryInfo = syscall.NewLazyDLL("psapi.dll").NewProc("GetProcessMemoryInfo")

// processRSS returns the working set of the test process (its resident
// memory) and the peak working set since the start, in bytes.
func processRSS() (rss, peak uint64, ok bool) {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, 0, false
	}
	var c processMemoryCounters
	c.cb = uint32(unsafe.Sizeof(c))
	if r, _, _ := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&c)), uintptr(c.cb)); r == 0 {
		return 0, 0, false
	}
	return uint64(c.WorkingSetSize), uint64(c.PeakWorkingSetSize), true
}
