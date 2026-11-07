// Package config loads and validates sources.json.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
)

// Source types understood by internal/sources.
const (
	TypeRSS = "rss"
	TypeHN  = "hn"
	TypeHDX = "hdx"
)

// Source is one place candidates come from.
type Source struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	URL      string `json:"url,omitempty"` // rss only
	Category string `json:"category"`      // "tech", "humanitarian", "global", ...
	MaxItems int    `json:"max_items,omitempty"`
	// ExcludeTitle drops items whose title matches this regular expression,
	// e.g. release candidates and nightly builds.
	ExcludeTitle string `json:"exclude_title,omitempty"`
	// TitlePrefix is prepended to every title (rss only). Release feeds title
	// items just "v0.40.15", which tells the model nothing without the project name.
	TitlePrefix string `json:"title_prefix,omitempty"`

	exclude *regexp.Regexp
}

// Excluded reports whether the title matches the source's exclusion pattern.
func (s Source) Excluded(title string) bool {
	return s.exclude != nil && s.exclude.MatchString(title)
}

// Config is the whole sources.json file.
type Config struct {
	LookbackDays int      `json:"lookback_days"` // only items newer than this are considered
	MaxPerRun    int      `json:"max_per_run"`   // ceiling on candidates sent to the model per run
	Sources      []Source `json:"sources"`
}

// Load reads and validates a config file. Unset numbers get defaults.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.LookbackDays <= 0 {
		c.LookbackDays = 14
	}
	if c.MaxPerRun <= 0 {
		c.MaxPerRun = 150
	}
	if len(c.Sources) == 0 {
		return nil, fmt.Errorf("%s: no sources configured", path)
	}
	seen := map[string]bool{}
	for i := range c.Sources {
		s := &c.Sources[i]
		if s.Name == "" || s.Category == "" {
			return nil, fmt.Errorf("source %d: name and category are required", i)
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("source %q appears twice", s.Name)
		}
		seen[s.Name] = true
		switch s.Type {
		case TypeRSS:
			if s.URL == "" {
				return nil, fmt.Errorf("source %q: rss needs a url", s.Name)
			}
		case TypeHN, TypeHDX:
		default:
			return nil, fmt.Errorf("source %q: unknown type %q", s.Name, s.Type)
		}
		if s.MaxItems <= 0 {
			s.MaxItems = 10
		}
		if s.ExcludeTitle != "" {
			re, err := regexp.Compile(s.ExcludeTitle)
			if err != nil {
				return nil, fmt.Errorf("source %q: exclude_title: %w", s.Name, err)
			}
			s.exclude = re
		}
	}
	return &c, nil
}
