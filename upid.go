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

// backupTaskKey is what the worker ID of a backup task says about its group:
// "<store>:<type>/<id>", without the namespace.
type backupTaskKey struct {
	store, backupType, backupID string
}

func parseBackupWorkerID(workerID string) (backupTaskKey, bool) {
	store, group, ok := strings.Cut(workerID, ":")
	if !ok {
		return backupTaskKey{}, false
	}
	backupType, backupID, ok := strings.Cut(group, "/")
	if !ok || store == "" || backupType == "" || backupID == "" {
		return backupTaskKey{}, false
	}
	return backupTaskKey{store, backupType, backupID}, true
}

// backupLogStart starts the line in the log of a backup task that names the
// snapshot, e.g. `starting new backup on datastore 'store1' from
// ::ffff:10.0.0.1: "ns/a/ns/b/vm/100/2026-10-18T20:15:02Z"` (older PBS
// versions leave out "from <ip>").
const backupLogStart = "starting new backup on datastore '"

// parseBackupLogNamespace returns the namespace of the snapshot that a line
// of the log of the backup task of key names, "" for the root namespace.
func parseBackupLogNamespace(line string, key backupTaskKey) (string, bool) {
	_, rest, ok := strings.Cut(line, backupLogStart)
	if !ok || !strings.HasPrefix(rest, key.store+"'") {
		return "", false
	}
	i := strings.LastIndex(rest, `: "`)
	if i < 0 {
		return "", false
	}
	path, ok := strings.CutSuffix(strings.TrimSpace(rest[i+3:]), `"`)
	if !ok {
		return "", false
	}
	// the path is the namespace as "ns/<name>" per level, then
	// "<type>/<id>/<time>"
	parts := strings.Split(path, "/")
	var ns []string
	for len(parts) > 3 && parts[0] == "ns" && parts[1] != "" {
		ns = append(ns, parts[1])
		parts = parts[2:]
	}
	if len(parts) != 3 || parts[0] != key.backupType || parts[1] != key.backupID {
		return "", false
	}
	return strings.Join(ns, "/"), true
}

// storeWorkerTypes have the datastore as first part of their worker ID.
var storeWorkerTypes = map[string]bool{
	// "sync" is a sync started by hand, its worker ID is the local datastore
	"sync":   true,
	"backup": true, "reader": true, "prune": true, "prunejob": true, "garbage_collection": true,
	"verificationjob": true, "verify": true, "verify_group": true, "verify_snapshot": true,
	"mount-device": true, "unmount-device": true, "create-datastore": true, "delete-datastore": true,
	"move-group": true, "tape-backup": true, "tape-backup-job": true,
}

// taskStore returns the datastore a task worked on, "" if it has none.
func taskStore(t taskItem) string {
	if t.WorkerType == "syncjob" {
		id, _ := parseSyncWorkerID(t.WorkerID)
		return id.store
	}
	if storeWorkerTypes[t.WorkerType] {
		store, _, _ := strings.Cut(t.WorkerID, ":")
		return store
	}
	return ""
}
