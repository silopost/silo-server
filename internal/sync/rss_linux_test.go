package sync_test

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// processRSS returns the resident set of the test process (VmRSS) and its
// peak (VmHWM) from /proc/self/status, in bytes.
func processRSS() (rss, peak uint64, ok bool) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, found := strings.Cut(sc.Text(), ":")
		if !found || (k != "VmRSS" && k != "VmHWM") {
			continue
		}
		kb, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		if err != nil {
			return 0, 0, false
		}
		if k == "VmRSS" {
			rss = kb << 10
		} else {
			peak = kb << 10
		}
	}
	return rss, peak, rss > 0
}
