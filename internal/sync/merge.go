package sync

import (
	"bytes"
	"encoding/json"
	"path"
	"strings"
	"time"
)

// jsonlLine is one line of an append-mostly JSONL file (a session transcript
// or the prompt history) with the identity and time used to merge it.
type jsonlLine struct {
	raw []byte    // line bytes without the trailing newline
	id  string    // "uuid" field when present, else the raw bytes
	ts  time.Time // "timestamp" field (RFC 3339 string or Unix millis); zero if absent
}

func parseJSONL(data []byte) []jsonlLine {
	var lines []jsonlLine
	var prev time.Time
	for _, raw := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		l := jsonlLine{raw: raw, id: "b:" + string(raw)}
		var probe struct {
			UUID      string          `json:"uuid"`
			Timestamp json.RawMessage `json:"timestamp"`
		}
		if json.Unmarshal(raw, &probe) == nil {
			if probe.UUID != "" {
				l.id = "u:" + probe.UUID
			}
			l.ts = parseLineTime(probe.Timestamp)
		}
		// Lines without a time stick to their predecessor when merging.
		if l.ts.IsZero() {
			l.ts = prev
		}
		prev = l.ts
		lines = append(lines, l)
	}
	return lines
}

func parseLineTime(raw json.RawMessage) time.Time {
	if len(raw) == 0 {
		return time.Time{}
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t
		}
		return time.Time{}
	}
	var ms float64
	if json.Unmarshal(raw, &ms) == nil && ms > 0 {
		return time.UnixMilli(int64(ms))
	}
	return time.Time{}
}

// MergeJSONL returns the union of two versions of an append-mostly JSONL
// file, so a conversation continued on two Macs before they synced keeps every
// message from both instead of one side overwriting the other.
//
// Lines are identified by their "uuid" (conversation entries) or, failing
// that, by their exact bytes. The shared history keeps local order. The two
// divergent tails are interleaved by timestamp, so the newest entry ends the
// file whichever device wrote it; Claude Code rebuilds the conversation tree
// from parentUuid links, so the other device's turns survive as a branch.
// changed is false when remote holds nothing local lacks.
func MergeJSONL(local, remote []byte) (merged []byte, changed bool) {
	loc := parseJSONL(local)
	rem := parseJSONL(remote)

	localIDs := make(map[string]bool, len(loc))
	for _, l := range loc {
		localIDs[l.id] = true
	}
	remoteIDs := make(map[string]bool, len(rem))
	var remoteOnly []jsonlLine
	for _, r := range rem {
		if remoteIDs[r.id] {
			continue
		}
		remoteIDs[r.id] = true
		if !localIDs[r.id] {
			remoteOnly = append(remoteOnly, r)
		}
	}
	if len(remoteOnly) == 0 {
		return local, false
	}

	// The local tail starts after the last line both sides share.
	split := 0
	for i, l := range loc {
		if remoteIDs[l.id] {
			split = i + 1
		}
	}

	var out bytes.Buffer
	out.Grow(len(local) + len(remote))
	emit := func(l jsonlLine) {
		out.Write(l.raw)
		out.WriteByte('\n')
	}
	for _, l := range loc[:split] {
		emit(l)
	}
	tail := loc[split:]
	i, j := 0, 0
	for i < len(tail) || j < len(remoteOnly) {
		// Ties go to local, keeping this device's order stable.
		if j >= len(remoteOnly) || (i < len(tail) && !remoteOnly[j].ts.Before(tail[i].ts)) {
			emit(tail[i])
			i++
		} else {
			emit(remoteOnly[j])
			j++
		}
	}
	return out.Bytes(), true
}

// IsMergeableJSONL reports whether a sync-namespace path is an append-mostly
// JSONL file that MergeJSONL can union safely.
func IsMergeableJSONL(npath string) bool {
	if npath == "history.jsonl" {
		return true
	}
	return strings.HasPrefix(npath, "projects/") && path.Ext(npath) == ".jsonl"
}

// desktopMetaStamp returns the activity time of a desktop session record, the
// newest of lastActivityAt and lastTurnEdgeAt in Unix millis (0 if unparsable).
func desktopMetaStamp(data []byte) int64 {
	var m struct {
		LastActivityAt json.Number `json:"lastActivityAt"`
		LastTurnEdgeAt json.Number `json:"lastTurnEdgeAt"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if dec.Decode(&m) != nil {
		return 0
	}
	a, _ := m.LastActivityAt.Int64()
	b, _ := m.LastTurnEdgeAt.Int64()
	if b > a {
		return b
	}
	return a
}

// PreferRemoteDesktopMeta decides a both-sides-changed desktop session record:
// the record with the newer activity wins, and a tie goes to remote so every
// device converges on the version that reached the bucket first.
func PreferRemoteDesktopMeta(local, remote []byte) bool {
	return desktopMetaStamp(remote) >= desktopMetaStamp(local)
}
