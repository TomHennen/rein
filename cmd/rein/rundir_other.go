//go:build !linux && !darwin

package main

import (
	"os"
	"time"
)

const runDirSweepMinAge = time.Hour

// No flock here: no lock, and the sweep never removes anything.
func lockRunDir(string) (*os.File, error) { return nil, nil }

func sweepStaleRunDirs(string, time.Duration, time.Time) {}
