package types

import (
	"strconv"
	"strings"
)

// ProcessSummary counts programs by high-level state for ctl / UI footers.
type ProcessSummary struct {
	Total    int
	Running  int
	Starting int
	Stopping int
	Stopped  int
	Exited   int
	Backoff  int
	Fatal    int
	Other    int
}

// SummarizeProcessInfos aggregates Statename counts.
func SummarizeProcessInfos(infos []ProcessInfo) ProcessSummary {
	var s ProcessSummary
	s.Total = len(infos)
	for i := range infos {
		switch strings.ToUpper(infos[i].Statename) {
		case "RUNNING":
			s.Running++
		case "STARTING":
			s.Starting++
		case "STOPPING":
			s.Stopping++
		case "STOPPED":
			s.Stopped++
		case "EXITED":
			s.Exited++
		case "BACKOFF":
			s.Backoff++
		case "FATAL":
			s.Fatal++
		default:
			s.Other++
		}
	}
	return s
}

// Format returns a compact one-line summary, e.g. "4 programs: 2 running, 1 stopped, 1 fatal".
func (s ProcessSummary) Format() string {
	if s.Total == 0 {
		return "0 programs"
	}
	parts := make([]string, 0, 8)
	add := func(n int, label string) {
		if n > 0 {
			parts = append(parts, strconv.Itoa(n)+" "+label)
		}
	}
	add(s.Running, "running")
	add(s.Starting, "starting")
	add(s.Stopping, "stopping")
	add(s.Stopped, "stopped")
	add(s.Exited, "exited")
	add(s.Backoff, "backoff")
	add(s.Fatal, "fatal")
	add(s.Other, "other")
	if len(parts) == 0 {
		return strconv.Itoa(s.Total) + " programs"
	}
	return strconv.Itoa(s.Total) + " programs: " + strings.Join(parts, ", ")
}
