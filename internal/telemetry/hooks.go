package telemetry

import (
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/spool"
)

const maxHookFile = 8 << 20

// ReadHookFile reads the hook counter file of one UTC day from the
// rendezvous directory, as doctor reads it. ok is false when the file is
// missing, unreadable or over the size cap.
func ReadHookFile(rv *os.Root, day time.Time) (raw []byte, ok bool) {
	dir, err := spool.OpenServerChild(rv, "hooks", false)
	if err != nil {
		return nil, false
	}
	defer dir.Close()
	file, err := spool.OpenServerFile(dir, day.UTC().Format("20060102"), os.O_RDONLY)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	raw, err = io.ReadAll(io.LimitReader(file, maxHookFile+1))
	if err != nil || len(raw) > maxHookFile {
		return nil, false
	}
	return raw, true
}

// addHookCounts counts each complete record "adapter event outcome ms" of a
// registered adapter and outcome, by event class and latency bucket.
// Anything else is skipped, as doctor skips it.
func addHookCounts(counts map[string]int64, raw []byte) {
	records := string(raw)
	for records != "" {
		line, rest, complete := strings.Cut(records, "\n")
		if !complete {
			break
		}
		records = rest
		fields := strings.Fields(line)
		if len(line) >= 64 || len(fields) != 4 {
			continue
		}
		adapter, outcome := fields[0], fields[2]
		ms, err := strconv.Atoi(fields[3])
		if err != nil || ms < 0 || !slices.Contains(adapters, adapter) || !slices.Contains(hookOutcomes, outcome) {
			continue
		}
		class, ok := hookClasses[fields[1]]
		if !ok {
			class = "other"
		}
		counts["hook."+adapter+"."+class+"."+outcome]++
		counts["hook_ms."+adapter+"."+class+"."+hookMSBucket(ms)]++
	}
}

func hookMSBucket(ms int) string {
	for i, b := range hookMSBounds {
		if ms < b {
			return hookMSBuckets[i]
		}
	}
	return hookMSBuckets[len(hookMSBuckets)-1]
}
