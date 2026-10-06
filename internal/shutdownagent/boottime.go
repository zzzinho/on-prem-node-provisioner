package shutdownagent

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// HostBootTime reports when the host kernel last booted, from the btime line of
// /proc/stat. btime is kernel-wide, so it is the host's boot time even read from
// inside the agent's container.
func HostBootTime() (time.Time, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()
	return parseBootTime(f)
}

// parseBootTime reads the "btime <unix seconds>" line of a /proc/stat stream.
func parseBootTime(r io.Reader) (time.Time, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || fields[0] != "btime" {
			continue
		}
		secs, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse btime %q: %w", fields[1], err)
		}
		return time.Unix(secs, 0), nil
	}
	if err := sc.Err(); err != nil {
		return time.Time{}, err
	}
	return time.Time{}, fmt.Errorf("no btime line in /proc/stat")
}
