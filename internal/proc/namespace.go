package proc

import (
	"strconv"
	"strings"
)

// PIDNamespaceBoot returns the boot ID from a valid Linux PID namespace tag.
func PIDNamespaceBoot(tag string) string {
	bootID, inode, ok := strings.Cut(tag, ":")
	if !ok || len(bootID) != 36 || bootID[8] != '-' || bootID[13] != '-' || bootID[18] != '-' || bootID[23] != '-' {
		return ""
	}
	digits := strings.ReplaceAll(bootID, "-", "")
	if len(digits) != 32 || strings.Trim(digits, "0123456789abcdef") != "" {
		return ""
	}
	number, err := strconv.ParseUint(inode, 10, 64)
	if err != nil || number == 0 || strconv.FormatUint(number, 10) != inode {
		return ""
	}
	return bootID
}
