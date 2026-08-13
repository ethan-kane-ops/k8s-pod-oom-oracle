package cgroup

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// maxValue is the literal cgroup v2 uses for "no limit".
const maxValue = "max"

// ParseUint reads a single unsigned integer from a cgroup file body.
//
// Kernel files carry a trailing newline and, on some controllers, padding.
// An empty body is treated as zero: a few controllers emit nothing rather than
// a value when the feature is compiled out.
func ParseUint(data []byte) (uint64, error) {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return 0, nil
	}
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing %q as an unsigned integer: %w", text, err)
	}
	return value, nil
}

// ParseLimit reads a memory ceiling, mapping the cgroup v2 "max" literal onto
// Unlimited.
func ParseLimit(data []byte) (uint64, error) {
	text := strings.TrimSpace(string(data))
	if text == maxValue {
		return Unlimited, nil
	}
	value, err := ParseUint(data)
	if err != nil {
		return 0, err
	}
	return normaliseLimit(value), nil
}

// ParseKeyValue reads the "key value" line format shared by memory.stat and
// memory.events.
//
// Unparseable values are skipped rather than failing the whole read: the kernel
// adds fields between releases, and one unknown field must not blind the daemon
// to the rest of the file.
func ParseKeyValue(data []byte) (map[string]uint64, error) {
	fields := make(map[string]uint64)

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		key, rawValue, found := strings.Cut(strings.TrimSpace(scanner.Text()), " ")
		if !found || key == "" {
			continue
		}
		value, err := strconv.ParseUint(strings.TrimSpace(rawValue), 10, 64)
		if err != nil {
			continue
		}
		fields[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning key/value content: %w", err)
	}

	return fields, nil
}

// PSILine is one pressure stall row: the share of a window during which tasks
// were stalled, plus a monotonic total in microseconds.
type PSILine struct {
	Avg10  float64 `json:"avg10"`
	Avg60  float64 `json:"avg60"`
	Avg300 float64 `json:"avg300"`
	Total  uint64  `json:"total"`
}

// PSI is the parsed content of memory.pressure.
//
// Some measures time when at least one task stalled; Full measures time when
// every task stalled. Full is the stronger OOM signal, since it means no work
// progressed at all.
type PSI struct {
	Some PSILine `json:"some"`
	Full PSILine `json:"full"`
}

// ParsePSI reads pressure stall information.
//
// The format is one row per scope:
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=0
//	full avg10=0.00 avg60=0.00 avg300=0.00 total=0
//
// The root cgroup omits the "full" row, so its absence is not an error.
func ParsePSI(data []byte) (PSI, error) {
	var psi PSI

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}

		var target *PSILine
		switch fields[0] {
		case "some":
			target = &psi.Some
		case "full":
			target = &psi.Full
		default:
			continue
		}

		line, err := parsePSIFields(fields[1:])
		if err != nil {
			return PSI{}, fmt.Errorf("parsing %q pressure row: %w", fields[0], err)
		}
		*target = line
	}
	if err := scanner.Err(); err != nil {
		return PSI{}, fmt.Errorf("scanning pressure content: %w", err)
	}

	return psi, nil
}

func parsePSIFields(fields []string) (PSILine, error) {
	var line PSILine

	for _, field := range fields {
		key, rawValue, found := strings.Cut(field, "=")
		if !found {
			continue
		}

		if key == "total" {
			total, err := strconv.ParseUint(rawValue, 10, 64)
			if err != nil {
				return PSILine{}, fmt.Errorf("parsing total %q: %w", rawValue, err)
			}
			line.Total = total
			continue
		}

		value, err := strconv.ParseFloat(rawValue, 64)
		if err != nil {
			return PSILine{}, fmt.Errorf("parsing %s %q: %w", key, rawValue, err)
		}
		switch key {
		case "avg10":
			line.Avg10 = value
		case "avg60":
			line.Avg60 = value
		case "avg300":
			line.Avg300 = value
		}
	}

	return line, nil
}
