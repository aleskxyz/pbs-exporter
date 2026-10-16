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

// parseUPIDWorker returns the worker type and the (unescaped) worker ID of a
// UPID, e.g. "verificationjob" and "store1:v-1".
func parseUPIDWorker(upid string) (workerType, workerID string, ok bool) {
	fields := strings.Split(upid, ":")
	if len(fields) < 9 || fields[0] != "UPID" || fields[6] == "" {
		return "", "", false
	}
	return fields[6], unescapeUPIDField(fields[7]), true
}

// unescapeUPIDField reverts the systemd style escaping of the worker ID in a
// UPID: "-" stands for "/", "\xNN" for the byte NN.
func unescapeUPIDField(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '-':
			b.WriteByte('/')
		case s[i] == '\\' && i+3 < len(s) && s[i+1] == 'x':
			if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
			b.WriteByte(s[i])
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// syncWorkerID is the worker ID of a syncjob task:
// "<remote>:<remote-store>:<local-store>:<local-ns>:<job-id>".
type syncWorkerID struct {
	remote, remoteStore, store, ns, jobID string
}

func parseSyncWorkerID(workerID string) (syncWorkerID, bool) {
	f := strings.SplitN(workerID, ":", 5)
	if len(f) != 5 || f[4] == "" {
		return syncWorkerID{}, false
	}
	remote := f[0]
	// a sync job between local datastores has no remote, its worker ID has "-"
	if remote == "-" {
		remote = ""
	}
	return syncWorkerID{remote: remote, remoteStore: f[1], store: f[2], ns: f[3], jobID: f[4]}, true
}
