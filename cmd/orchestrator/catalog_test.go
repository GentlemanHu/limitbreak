package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	catHost = "local_0a1b2c3d-1111-4222-8333-444455556666"
	catCLI  = "11111111-2222-4333-8444-555555555555"
)

func writeCatalogFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestReadCatalogKeepsOnlyMetadata(t *testing.T) {
	root := filepath.Join(t.TempDir(), "claude-code-sessions")
	profile := filepath.Join(root, "acct", "profile")
	writeCatalogFile(t, profile, catHost+".json", `{"sessionId":"`+catHost+`","cliSessionId":"`+catCLI+`","cwd":"/Users/u/p","title":"  你是什么 \n","model":"claude-opus-4-7-anti[1m]","isArchived":false,"lastActivityAt":1760000000123,"initialMessage":"secret prompt","permissionMode":"default"}`)
	entries, complete := readCatalog([]string{root, filepath.Join(t.TempDir(), "absent")})
	if !complete || len(entries) != 1 {
		t.Fatalf("complete=%v entries=%+v", complete, entries)
	}
	e := entries[0]
	if e.HostSessionID != catHost || e.CLISessionID != catCLI || e.Cwd != "/Users/u/p" || e.Title != "你是什么" || e.Model != "claude-opus-4-7-anti[1m]" || e.Archived {
		t.Fatalf("wrong entry %+v", e)
	}
	if !e.LastActivityAt.Equal(time.UnixMilli(1760000000123)) {
		t.Fatalf("wrong activity %v", e.LastActivityAt)
	}
}

func TestReadCatalogSkipsUnsafeOrInvalidFiles(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "claude-code-sessions")
	profile := filepath.Join(root, "acct", "profile")
	other := "local_9f8e7d6c-1111-4222-8333-444455556666"
	// Name does not match its sessionId.
	writeCatalogFile(t, profile, other+".json", `{"sessionId":"`+catHost+`","cliSessionId":"`+catCLI+`"}`)
	// Invalid CLI id.
	writeCatalogFile(t, profile, catHost+".json", `{"sessionId":"`+catHost+`","cliSessionId":"../x"}`)
	// Symlinked file and symlinked directory.
	outside := filepath.Join(base, "outside")
	writeCatalogFile(t, outside, "local_aaaaaaaa-1111-4222-8333-444455556666.json", `{"sessionId":"local_aaaaaaaa-1111-4222-8333-444455556666","cliSessionId":"`+catCLI+`"}`)
	if err := os.Symlink(filepath.Join(outside, "local_aaaaaaaa-1111-4222-8333-444455556666.json"), filepath.Join(profile, "local_aaaaaaaa-1111-4222-8333-444455556666.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "acct", "linked")); err != nil {
		t.Fatal(err)
	}
	// Null title is accepted as empty.
	good := "local_bbbbbbbb-1111-4222-8333-444455556666"
	writeCatalogFile(t, profile, good+".json", `{"sessionId":"`+good+`","cliSessionId":"`+catCLI+`","title":null}`)
	entries, complete := readCatalog([]string{root})
	if !complete || len(entries) != 1 || entries[0].HostSessionID != good || entries[0].Title != "" {
		t.Fatalf("complete=%v entries=%+v", complete, entries)
	}
}

func TestReadCatalogWithoutRootsIsIncomplete(t *testing.T) {
	if entries, complete := readCatalog([]string{filepath.Join(t.TempDir(), "absent")}); complete || len(entries) != 0 {
		t.Fatalf("missing roots must not look like an empty list: %v %v", complete, entries)
	}
}
