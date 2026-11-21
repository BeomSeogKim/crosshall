//go:build linux

package proc

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// processParent returns a running pid's command name and parent pid from
// /proc/<pid>/stat: "pid (comm) state ppid ...", where comm may hold spaces
// and parentheses.
func processParent(pid int) (string, int, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", 0, err
	}
	stat := string(raw)
	open, close := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
	if open < 0 || close < open {
		return "", 0, errors.New("malformed process stat")
	}
	fields := strings.Fields(stat[close+1:])
	if len(fields) < 2 {
		return "", 0, errors.New("malformed process stat")
	}
	parent, err := strconv.Atoi(fields[1])
	return stat[open+1 : close], parent, err
}
