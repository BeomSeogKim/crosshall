package setup

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/spool"
)

// The owner's explicit "no" to an adapter offer, kept apart from the install
// journal and the runtime policy. It only keeps the installer from offering
// that adapter again; it never counts as consent to anything.
const offersName = "setup-offers.json"

type answer int

const (
	answerSkip answer = iota // Enter, end of input, an interruption or an error
	answerYes
	answerNo
)

type offerPrefs struct {
	Version int                      `json:"v"`
	Seq     int64                    `json:"seq"`
	Vendors map[string]offerDecision `json:"vendors"`
}

// offerDecision is a vendor's latest explicit answer. Seq orders decisions
// across runs: a run may only replace the decision it saw before asking.
type offerDecision struct {
	Answer string `json:"answer"` // yes|no
	Seq    int64  `json:"seq"`
	AtMS   int64  `json:"at_ms"`
}

// declinedOffers reads the remembered declines without creating anything.
// A missing state directory or file means none; a malformed file is an error.
func (s Service) declinedOffers() (map[string]bool, error) {
	prefs, err := s.readOfferPrefs()
	if err != nil {
		return nil, err
	}
	declined := map[string]bool{}
	for adapter, d := range prefs.Vendors {
		declined[adapter] = d.Answer == "no"
	}
	return declined, nil
}

// offerSnapshot records which decision of each vendor a run saw before
// asking, so its answers cannot overwrite a newer one.
func (s Service) offerSnapshot() (map[string]int64, error) {
	prefs, err := s.readOfferPrefs()
	if err != nil {
		return nil, err
	}
	return prefs.snapshot(), nil
}

func (prefs offerPrefs) snapshot() map[string]int64 {
	seen := map[string]int64{}
	for adapter, d := range prefs.Vendors {
		seen[adapter] = d.Seq
	}
	return seen
}

func (s Service) readOfferPrefs() (offerPrefs, error) {
	state, err := openSetupState(s.StateRoot, false)
	if errors.Is(err, os.ErrNotExist) {
		return offerPrefs{Version: 1, Vendors: map[string]offerDecision{}}, nil
	}
	if err != nil {
		return offerPrefs{}, err
	}
	defer state.Close()
	return readOffers(state)
}

// rememberAnswers records each explicit answer under the setup lock, after
// re-reading the file, unless another run decided about that vendor after
// seen was taken: the newer decision stands. Skips change nothing; when
// every answer is a skip, nothing is opened or written.
func (s Service) rememberAnswers(answers map[string]answer, seen map[string]int64, now time.Time) error {
	changes := false
	for _, a := range answers {
		changes = changes || a != answerSkip
	}
	if !changes {
		return nil
	}
	return s.WithSetupLock(func(state *os.Root) error {
		prefs, err := readOffers(state)
		if err != nil {
			return err
		}
		changed := false
		for adapter, a := range answers {
			if a == answerSkip || prefs.Vendors[adapter].Seq != seen[adapter] {
				continue
			}
			value := "yes"
			if a == answerNo {
				value = "no"
			}
			prefs.Seq++
			prefs.Vendors[adapter] = offerDecision{Answer: value, Seq: prefs.Seq, AtMS: now.UnixMilli()}
			changed = true
		}
		if !changed {
			return nil
		}
		data, err := json.Marshal(prefs)
		if err != nil {
			return err
		}
		stage, _, err := stageConfig(state, data, 0600)
		if err != nil {
			return err
		}
		defer state.Remove(stage)
		if err := state.Rename(stage, offersName); err != nil {
			return err
		}
		return syncConfigDir(state)
	})
}

func readOffers(state *os.Root) (offerPrefs, error) {
	prefs := offerPrefs{Version: 1, Vendors: map[string]offerDecision{}}
	file, err := spool.OpenServerFile(state, offersName, os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return prefs, nil
	}
	if err != nil {
		return prefs, err
	}
	defer file.Close()
	const maxOffers = 64 << 10
	data, err := io.ReadAll(io.LimitReader(file, maxOffers+1))
	if err != nil {
		return prefs, err
	}
	prefs.Version, prefs.Vendors = 0, nil
	if len(data) > maxOffers || json.Unmarshal(data, &prefs) != nil || prefs.Version != 1 || prefs.Vendors == nil {
		return prefs, errors.New("invalid setup offer preferences")
	}
	return prefs, nil
}
