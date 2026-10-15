package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// titleWindow bounds how much of the transcript tail is scanned. Claude Code
// appends the session's title record after every turn, so the latest one is
// always near the end.
const titleWindow = 4 << 20

// transcriptTitle returns the session title Claude records in its own
// transcript ("custom-title" records, the name shown in the Claude sidebar).
// Only those title records are decoded; prompts and answers are skipped
// without being parsed or kept.
func transcriptTitle(path, sessionID string) string {
	if !filepath.IsAbs(path) || filepath.Base(path) != sessionID+".jsonl" {
		return ""
	}
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st.Size() > titleWindow {
		if _, err := f.Seek(st.Size()-titleWindow, io.SeekStart); err != nil {
			return ""
		}
	}
	marker := []byte(`"custom-title"`)
	title := ""
	r := bufio.NewReaderSize(io.LimitReader(f, titleWindow), 64<<10)
	for {
		line, err := r.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			// Overlong lines are conversation content, never title records.
			for err == bufio.ErrBufferFull {
				_, err = r.ReadSlice('\n')
			}
			if err != nil {
				break
			}
			continue
		}
		if bytes.Contains(line, marker) {
			var record struct {
				Type        string `json:"type"`
				CustomTitle string `json:"customTitle"`
				SessionID   string `json:"sessionId"`
			}
			if json.Unmarshal(line, &record) == nil && record.Type == "custom-title" && (record.SessionID == "" || record.SessionID == sessionID) {
				if t := cleanTitle(record.CustomTitle); t != "" {
					title = t
				}
			}
		}
		if err != nil {
			break
		}
	}
	return title
}

func cleanTitle(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if runes := []rune(s); len(runes) > 200 {
		s = string(runes[:200])
	}
	return s
}
