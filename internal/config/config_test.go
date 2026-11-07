package config

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sources.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAppliesDefaultsAndCompilesExclude(t *testing.T) {
	c, err := Load(write(t, `{"sources":[{"name":"a","type":"rss","url":"https://e.com/f","category":"tech","exclude_title":"(?i)beta"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.LookbackDays != 14 || c.MaxPerRun != 150 || c.Sources[0].MaxItems != 10 {
		t.Fatalf("defaults not applied: %+v", c)
	}
	if !c.Sources[0].Excluded("v1.0 BETA") || c.Sources[0].Excluded("v1.0") {
		t.Fatal("exclude pattern misbehaves")
	}
}

func TestLoadRejectsBadConfigs(t *testing.T) {
	bad := map[string]string{
		"unknown type": `{"sources":[{"name":"a","type":"nope","category":"tech"}]}`,
		"rss no url":   `{"sources":[{"name":"a","type":"rss","category":"tech"}]}`,
		"no category":  `{"sources":[{"name":"a","type":"hn"}]}`,
		"duplicate":    `{"sources":[{"name":"a","type":"hn","category":"t"},{"name":"a","type":"hn","category":"t"}]}`,
		"bad regex":    `{"sources":[{"name":"a","type":"hn","category":"t","exclude_title":"("}]}`,
		"empty":        `{"sources":[]}`,
		"not json":     `nope`,
	}
	for name, body := range bad {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
