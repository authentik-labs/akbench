package api

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// killSessionMembers SIGKILLs every process still in session sid. Killing
// the shell's process group alone misses jobs bash moved into groups of
// their own (`kubectl logs -f ... &`, anything nohup'd), which would
// otherwise outlive the browser tab for as long as the pod runs.
func killSessionMembers(sid int) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if processSession(pid) == sid {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// processSession reads field 6 (session) of /proc/<pid>/stat. The command
// name (field 2) is parenthesised and may itself contain spaces or
// parentheses, so fields are counted from the last ')'.
func processSession(pid int) int {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return -1
	}
	stat := string(raw)
	_, rest, ok := strings.CutLast(stat, ")")
	if !ok {
		return -1
	}
	// After ')': state ppid pgrp session ...
	fields := strings.Fields(rest)
	if len(fields) < 4 {
		return -1
	}
	sid, err := strconv.Atoi(fields[3])
	if err != nil {
		return -1
	}
	return sid
}
