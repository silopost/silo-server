package config

import (
	"fmt"
	"strconv"
	"strings"
)

// ByteSize is a size in bytes, parsed from values such as "50MB" or "0".
// Units are binary (1KB = 1024 bytes); a bare number means bytes.
type ByteSize int64

// ByteRate is a throughput in bytes per second, parsed from values such as "5MB/s".
type ByteRate int64

// UnmarshalText implements encoding.TextUnmarshaler.
func (b *ByteSize) UnmarshalText(text []byte) error {
	n, err := parseSize(string(text))
	*b = ByteSize(n)
	return err
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (r *ByteRate) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/s"), "/S")
	n, err := parseSize(s)
	*r = ByteRate(n)
	return err
}

var sizeUnits = []struct {
	suffix string
	mult   int64
}{
	// Longer suffixes first so "MB" is not mistaken for "B".
	{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1},
}

func parseSize(s string) (int64, error) {
	orig := s
	s = strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	for _, u := range sizeUnits {
		if strings.HasSuffix(s, u.suffix) {
			mult = u.mult
			s = strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || n > (1<<62)/mult {
		return 0, fmt.Errorf("invalid size %q (expected e.g. 50MB)", orig)
	}
	return n * mult, nil
}
