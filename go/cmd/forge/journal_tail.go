// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// journalTailArgs builds journalctl's argv for one unit's recent output (message text only). ok is false when
// the unit is not a well-formed systemd unit name, so nothing odd ever reaches journalctl's -u.
func journalTailArgs(unit string, since time.Time, maxLines int) ([]string, bool) {
	// systemdUnitRe allows a leading "-", which journalctl would take as an option (or swallow as -u's value).
	if !systemdUnitRe.MatchString(unit) || strings.HasPrefix(unit, "-") {
		return nil, false
	}
	if maxLines <= 0 || maxLines > 200 {
		maxLines = 12
	}
	args := []string{"--no-pager", "-o", "cat", "-n", strconv.Itoa(maxLines), "-u", unit}
	if !since.IsZero() {
		args = append(args, "--since", since.Format("2006-01-02 15:04:05"))
	}
	return args, true
}

// journalTail is engine.Deps.LogTail's production implementation: the last few lines a slot's unit logged since
// a load began, so a failed load can say why without anyone opening the journal. Read-only and bounded; it
// degrades to "" on any failure (the engine redacts and caps whatever it returns).
func journalTail(ctx context.Context, unit string, since time.Time, maxLines int) string {
	args, ok := journalTailArgs(unit, since, maxLines)
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "journalctl", args...).Output()
	if err != nil && len(out) == 0 {
		return ""
	}
	return string(out)
}
