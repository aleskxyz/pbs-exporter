package main

import (
	"strconv"
	"strings"
)

// parseUPIDStart returns the start time (unix seconds) of a task from its UPID
// "UPID:<node>:<pid>:<pstart>:<task-id>:<starttime>:<type>:<id>:<user>:",
// where the start time is hex. The worker ID escapes ":" as "\x3a", so
// splitting at ":" is safe.
func parseUPIDStart(upid string) (int64, bool) {
	fields := strings.Split(upid, ":")
	if len(fields) < 9 || fields[0] != "UPID" {
		return 0, false
	}
	start, err := strconv.ParseInt(fields[5], 16, 64)
	if err != nil || start <= 0 {
		return 0, false
	}
	return start, true
}
