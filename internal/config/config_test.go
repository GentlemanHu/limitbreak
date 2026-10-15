package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultAndPrivateConfig(t *testing.T) {
	base := t.TempDir()
	c := Default(base)
	c.Token = strings.Repeat("t", 64)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(c)
	path := filepath.Join(base, "config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Database != filepath.Join(base, "state.sqlite") {
		t.Fatal("default database incorrectly resolved")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("world-readable token config accepted")
	}
}

func TestNetworkAndPolicyValidation(t *testing.T) {
	c := Default(t.TempDir())
	c.Token = strings.Repeat("t", 64)
	for _, addr := range []string{"0.0.0.0:8318", "localhost:8318", "192.168.1.2:8318", ":8318"} {
		x := c
		x.Listen = addr
		if x.Validate() == nil {
			t.Fatalf("unsafe listen %s accepted", addr)
		}
	}
	for _, proxy := range []string{"http://public.example", "ftp://public.example", "https://user:pass@public.example"} {
		x := c
		x.ProxyURL = proxy
		if x.Validate() == nil {
			t.Fatalf("unsafe proxy accepted %s", proxy)
		}
	}
	p := c.Policy
	p.MaxPerAccount = p.MaxRunning + 1
	if ValidatePolicy(p) == nil {
		t.Fatal("invalid concurrency accepted")
	}
	p = c.Policy
	p.ContinueMessage = ""
	if ValidatePolicy(p) == nil {
		t.Fatal("empty instruction accepted")
	}
}

func TestSymlinkConfigRejected(t *testing.T) {
	base := t.TempDir()
	c := Default(base)
	c.Token = strings.Repeat("t", 64)
	data, _ := json.Marshal(c)
	path := filepath.Join(base, "config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "linked.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(link); err == nil {
		t.Fatal("symlink accepted")
	}
}
