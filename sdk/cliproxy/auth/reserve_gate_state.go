package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

// reserveStateVersion is the version of the reserve gate state file.
const reserveStateVersion = 1

// reserveStateFile is the reserve gate state file: per account ID and window ("5h",
// "7d"), the last known reading and the hold. It holds no credential data.
type reserveStateFile struct {
	Version   int                                      `json:"version"`
	UpdatedAt time.Time                                `json:"updated_at"`
	Accounts  map[string]map[string]reserveStateWindow `json:"accounts"`
}

type reserveStateWindow struct {
	Utilization float64   `json:"utilization"`
	ResetAt     int64     `json:"reset_at,omitempty"` // Unix seconds; 0 if unknown
	ObservedAt  time.Time `json:"observed_at"`
	Held        bool      `json:"held,omitempty"`
	HeldUntil   int64     `json:"held_until,omitempty"` // Unix seconds; 0 if unknown
}

// reserveStatePath returns the state file of an enabled gate: state-file, or
// DefaultReserveGateStateFileName in auth-dir. Empty means the gate keeps its state in
// memory only (disabled gate, or neither path configured).
func reserveStatePath(cfg *internalconfig.Config) string {
	if cfg == nil || !cfg.Routing.ReserveGate.Enabled {
		return ""
	}
	path := strings.TrimSpace(cfg.Routing.ReserveGate.StateFile)
	if path == "" {
		authDir := strings.TrimSpace(cfg.AuthDir)
		if authDir == "" {
			log.Warn("reserve gate: neither auth-dir nor routing.reserve-gate.state-file is set; readings and holds are not kept across restarts")
			return ""
		}
		path = filepath.Join(authDir, internalconfig.DefaultReserveGateStateFileName)
	}
	// ResolveAuthDir expands a leading ~ and cleans the path.
	resolved, errResolve := util.ResolveAuthDir(path)
	if errResolve != nil {
		log.Warnf("reserve gate: cannot resolve state file %s: %v; readings and holds are not kept across restarts", path, errResolve)
		return ""
	}
	return resolved
}

// useStateFile points the book at path. A new path is read into the book: entries the
// book already knows stay as they are, and entries whose window has reset are dropped.
// A missing or unreadable file leaves the book as it is, with a log line.
func (b *reserveHoldBook) useStateFile(path string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if path == b.path {
		return
	}
	b.path = path
	if path == "" {
		return
	}
	file, errRead := readReserveStateFile(path)
	if errRead != nil {
		if errors.Is(errRead, os.ErrNotExist) {
			log.Infof("reserve gate: no state file at %s yet; starting empty", path)
		} else {
			log.Warnf("reserve gate: ignoring state file %s: %v; starting empty", path, errRead)
		}
		return
	}
	loaded := 0
	for authID, windows := range file.Accounts {
		for window, entry := range windows {
			key := reserveKey{authID: authID, window: window}
			if _, known := b.readings[key]; known || !isReserveWindow(window) || authID == "" {
				continue
			}
			if math.IsNaN(entry.Utilization) || math.IsInf(entry.Utilization, 0) || entry.Utilization < 0 {
				continue
			}
			reading := reserveReading{utilization: entry.Utilization, resetAt: unixOrZero(entry.ResetAt), observedAt: entry.ObservedAt}
			if !reading.resetAt.IsZero() && !reading.resetAt.After(now) {
				continue
			}
			if b.readings == nil {
				b.readings = make(map[reserveKey]reserveReading)
			}
			b.readings[key] = reading
			if entry.Held {
				if b.holds == nil {
					b.holds = make(map[reserveKey]reserveHold)
				}
				b.holds[key] = reserveHold{held: true, until: unixOrZero(entry.HeldUntil)}
			}
			loaded++
		}
	}
	log.Infof("reserve gate: loaded %d window readings from state file %s", loaded, path)
}

func readReserveStateFile(path string) (reserveStateFile, error) {
	var file reserveStateFile
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		return file, errRead
	}
	if errJSON := json.Unmarshal(data, &file); errJSON != nil {
		return file, fmt.Errorf("parse: %w", errJSON)
	}
	if file.Version != reserveStateVersion {
		return file, fmt.Errorf("version %d, want %d", file.Version, reserveStateVersion)
	}
	return file, nil
}

// save writes the book to its state file: atomic replace, mode 0600. Readings whose
// window has reset are left out. A failed write is logged; the book stays in memory.
func (b *reserveHoldBook) save(now time.Time) {
	b.saveMu.Lock()
	defer b.saveMu.Unlock()
	b.mu.Lock()
	path := b.path
	file := reserveStateFile{Version: reserveStateVersion, UpdatedAt: now.UTC(), Accounts: make(map[string]map[string]reserveStateWindow)}
	for key, reading := range b.readings {
		if !reading.resetAt.IsZero() && !reading.resetAt.After(now) {
			continue
		}
		entry := reserveStateWindow{Utilization: reading.utilization, ResetAt: unixOrZeroOf(reading.resetAt), ObservedAt: reading.observedAt.UTC()}
		if hold, held := b.holds[key]; held && hold.held {
			entry.Held, entry.HeldUntil = true, unixOrZeroOf(hold.until)
		}
		if file.Accounts[key.authID] == nil {
			file.Accounts[key.authID] = make(map[string]reserveStateWindow)
		}
		file.Accounts[key.authID][key.window] = entry
	}
	b.mu.Unlock()
	if path == "" {
		return
	}
	if errWrite := writeReserveStateFile(path, file); errWrite != nil {
		log.Warnf("reserve gate: cannot write state file %s: %v", path, errWrite)
	}
}

func writeReserveStateFile(path string, file reserveStateFile) error {
	data, errMarshal := json.MarshalIndent(file, "", "  ")
	if errMarshal != nil {
		return errMarshal
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return errMkdir
	}
	tmpFile, errCreate := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if errCreate != nil {
		return errCreate
	}
	tmp := tmpFile.Name()
	errWrite := tmpFile.Chmod(0o600)
	if errWrite == nil {
		_, errWrite = tmpFile.Write(data)
	}
	if errWrite == nil {
		errWrite = tmpFile.Sync()
	}
	if errClose := tmpFile.Close(); errWrite == nil {
		errWrite = errClose
	}
	if errWrite == nil {
		errWrite = os.Rename(tmp, path)
	}
	if errWrite != nil {
		_ = os.Remove(tmp)
	}
	return errWrite
}

func isReserveWindow(window string) bool {
	for _, known := range reserveWindows {
		if window == known {
			return true
		}
	}
	return false
}

func unixOrZero(seconds int64) time.Time {
	if seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0)
}

func unixOrZeroOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
