package telemetryconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
)

func ReadSent(stateRoot, day string) ([]string, []byte, error) {
	days := []string{}
	telemetry, err := openTelemetryDir(stateRoot, false)
	if errors.Is(err, os.ErrNotExist) && day == "" {
		return days, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer telemetry.Close()
	sent, err := ident.OpenPrivateChild(telemetry, "sent")
	if errors.Is(err, os.ErrNotExist) && day == "" {
		return days, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer sent.Close()

	if day != "" {
		body, err := ident.ReadPrivateFile(sent, day+".json", 16*1024)
		if err == nil && !json.Valid(body) {
			err = fmt.Errorf("invalid sent usage report for %s", day)
		}
		return nil, body, err
	}
	entries, err := fs.ReadDir(sent.FS(), ".")
	if err != nil {
		return nil, nil, err
	}
	for _, entry := range entries {
		name, jsonFile := strings.CutSuffix(entry.Name(), ".json")
		if !jsonFile || !entry.Type().IsRegular() {
			continue
		}
		if _, err := time.Parse(time.DateOnly, name); err == nil {
			days = append(days, name)
		}
	}
	return days, nil, nil
}
