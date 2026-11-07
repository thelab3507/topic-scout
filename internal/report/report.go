// Package report renders digest.html, the landing page where results are
// viewed and considered. It is one self-contained file: styles and the Plus
// Jakarta Sans font are embedded, so it works offline and needs no server.
package report

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/thelab3507/topic-scout/internal/novelty"
	"github.com/thelab3507/topic-scout/internal/score"
	"github.com/thelab3507/topic-scout/internal/store"
)

//go:embed assets/digest.html.tmpl
var tmplSource string

//go:embed assets/digest.css
var css string

//go:embed assets/plus-jakarta-sans-latin-wght-normal.woff2
var fontWOFF2 []byte

// WindowDays is how far back the digest looks.
const WindowDays = 30

var page = template.Must(template.New("digest").Parse(tmplSource))

// Input is everything the digest shows.
type Input struct {
	Now           time.Time
	LastRun       *store.LastRun
	Records       []store.RecordLine
	Novelty       map[string]novelty.Evidence
	Decisions     map[string]store.DecisionRecord
	RubricVersion string
}

type pill struct {
	Label, Full string
	Value       int
	Class       string
}

type noveltyView struct {
	Headline string
	Query    string
	Skipped  string
	Recent   []novelty.Repo
	Top      []novelty.Repo
}

type card struct {
	ID, Title, URL, Source, Category string
	PublishedLabel                   string
	Pills                            []pill
	Total                            int
	Reason                           string
	Warnings                         []string
	Summary                          string
	Decision, Note                   string
	Novelty                          *noveltyView
	Attempts                         int
	Error                            string
	published                        time.Time
}

type banner struct{ Title, Body string }

type view struct {
	CSS                          template.CSS
	Banner                       *banner
	Shortlist, Unscored, Rest    []card
	Dismissed                    []card
	LastRunLabel, GeneratedLabel string
	RubricVersion                string
	WindowDays, MaxAttempts      int
}

// Render writes the digest HTML to w.
func Render(w io.Writer, in Input) error {
	var buf bytes.Buffer
	if err := page.Execute(&buf, build(in)); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// Write renders the digest to path, replacing it atomically.
func Write(path string, in Input) error {
	var buf bytes.Buffer
	if err := Render(&buf, in); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

var pillNames = [5][2]string{
	{"First", "First or different"},
	{"Question", "Testable question"},
	{"Timebox", "Fits the timebox"},
	{"Ships", "Ships publicly"},
	{"Community", "Reaches the right people"},
}

func build(in Input) view {
	v := view{
		CSS:            template.CSS(fontFace() + css),
		RubricVersion:  in.RubricVersion,
		WindowDays:     WindowDays,
		MaxAttempts:    store.MaxAttempts,
		GeneratedLabel: in.Now.Local().Format("2 Jan 2006 15:04"),
		LastRunLabel:   "no run yet",
	}
	if lr := in.LastRun; lr != nil {
		v.LastRunLabel = lr.FinishedAt.Local().Format("2 Jan, 15:04")
		if lr.Outcome != "ok" {
			v.Banner = &banner{Title: "The last run did not finish normally (" + lr.Outcome + ").", Body: lr.Message}
		}
	} else {
		v.Banner = &banner{Title: "No run recorded yet.", Body: "Run scout once, or wait for the scheduled task."}
	}

	since := in.Now.AddDate(0, 0, -WindowDays)
	for _, rec := range in.Records {
		if rec.RunAt.Before(since) {
			continue
		}
		c := toCard(rec, in)
		switch {
		case c.Decision == store.Dismissed:
			v.Dismissed = append(v.Dismissed, c)
		case rec.Result.Status != score.StatusScored:
			v.Unscored = append(v.Unscored, c)
		case rec.Result.Shortlisted || c.Decision == store.Picked:
			v.Shortlist = append(v.Shortlist, c)
		default:
			v.Rest = append(v.Rest, c)
		}
	}
	sort.SliceStable(v.Shortlist, func(i, j int) bool {
		a, b := v.Shortlist[i], v.Shortlist[j]
		if (a.Decision == store.Picked) != (b.Decision == store.Picked) {
			return a.Decision == store.Picked
		}
		if a.Total != b.Total {
			return a.Total > b.Total
		}
		return a.published.After(b.published)
	})
	sort.SliceStable(v.Rest, func(i, j int) bool { return v.Rest[i].Total > v.Rest[j].Total })
	sort.SliceStable(v.Unscored, func(i, j int) bool { return v.Unscored[i].published.After(v.Unscored[j].published) })
	return v
}

func toCard(rec store.RecordLine, in Input) card {
	c := rec.Candidate
	r := rec.Result
	out := card{
		ID: c.ID, Title: c.Title, URL: c.URL, Source: c.Source, Category: c.Category,
		PublishedLabel: c.Published.Local().Format("2 Jan 2006"),
		Summary:        c.Summary, published: c.Published,
		Attempts: r.Attempts, Error: r.Error, Warnings: r.Warnings,
	}
	if d, ok := in.Decisions[c.ID]; ok {
		out.Decision, out.Note = d.Decision, d.Note
	}
	if s := r.Scores; s != nil {
		out.Reason = s.Reason
		out.Total = s.Total()
		for i, val := range []int{s.FirstOrDifferent, s.TestableQuestion, s.FitsTimebox, s.ShipsPublicly, s.HasCommunity} {
			out.Pills = append(out.Pills, pill{Label: pillNames[i][0], Full: pillNames[i][1], Value: val, Class: pillClass(val)})
		}
	}
	if ev, ok := in.Novelty[c.ID]; ok {
		out.Novelty = &noveltyView{
			Headline: noveltyHeadline(ev), Query: ev.Query, Skipped: ev.Skipped, Recent: ev.Recent, Top: ev.Top,
		}
	}
	return out
}

func pillClass(v int) string {
	switch {
	case v >= score.ShortlistMin:
		return "badge-default"
	case v == 3:
		return "badge-secondary"
	default:
		return "badge-destructive"
	}
}

func noveltyHeadline(ev novelty.Evidence) string {
	if ev.Skipped != "" {
		return "GitHub novelty check: skipped"
	}
	return fmt.Sprintf("GitHub novelty check: %d repos in the last 30 days, %d all time", ev.RecentTotal, ev.AllTimeTotal)
}

// fontFace embeds the Latin subset of Plus Jakarta Sans Variable (SIL OFL 1.1,
// see assets/FONT-LICENSE.txt) so the page needs nothing from the network.
func fontFace() string {
	var b strings.Builder
	b.WriteString(`@font-face{font-family:"Plus Jakarta Sans Variable";font-style:normal;font-display:swap;font-weight:200 800;src:url(data:font/woff2;base64,`)
	b.WriteString(base64.StdEncoding.EncodeToString(fontWOFF2))
	b.WriteString(`) format("woff2-variations");}` + "\n")
	return b.String()
}
