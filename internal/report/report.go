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

	"github.com/thelab3507/topic-scout/internal/cluster"
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
	// Clusters is the latest weekly pattern pass; the zero value means none yet.
	Clusters cluster.File
	// Progress is non-empty while a run is still scoring. The page then says so
	// and refreshes itself; the final write of a run leaves it empty.
	Progress string
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

// ideaView is one idea as the template shows it: a kind badge, a bold title and
// a muted detail.
type ideaView struct {
	Kind, Class   string
	Title, Detail string
}

type patternItem struct{ Title, URL, Source string }

type patternView struct {
	Name, Tag, CountLabel string
	Forming, OpenQuestion string
	Ideas                 []ideaView
	Items                 []patternItem
}

type card struct {
	ID, Title, URL, Source, Category string
	PublishedLabel                   string
	Pills                            []pill
	Total                            int
	Reason                           string
	Gist                             string
	Tags                             []string
	Ideas                            []ideaView
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
	Progress                     string
	Patterns                     []patternView
	PatternsNote                 string
	PatternsLabel                string
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
	{"Hook", "Project hook"},
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
		Progress:       in.Progress,
	}
	if lr := in.LastRun; lr != nil {
		v.LastRunLabel = lr.FinishedAt.Local().Format("2 Jan, 15:04")
		if lr.Outcome != "ok" {
			v.Banner = &banner{Title: "The last run did not finish normally (" + lr.Outcome + ").", Body: lr.Message}
		}
	} else {
		v.Banner = &banner{Title: "No run recorded yet.", Body: "Run scout once, or wait for the scheduled task."}
	}
	if in.Progress != "" {
		v.Banner = nil // the previous run's outcome is not news while a new run is in flight
	}

	v.Patterns, v.PatternsNote, v.PatternsLabel = patterns(in.Clusters)

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
		out.Gist, out.Tags, out.Ideas = s.Gist, s.Tags, ideaViews(s.Ideas)
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

func ideaViews(ideas []score.Idea) []ideaView {
	var out []ideaView
	for _, i := range ideas {
		class := "badge-outline"
		switch i.Kind {
		case score.KindBuild:
			class = "badge-default"
		case score.KindContribute:
			class = "badge-secondary"
		}
		out = append(out, ideaView{Kind: i.Kind, Class: class, Title: i.Title, Detail: i.Detail})
	}
	return out
}

// patterns turns the latest pass into what the Patterns section shows. When
// there is none, the note says why.
func patterns(f cluster.File) (views []patternView, note, label string) {
	for _, c := range f.Clusters {
		pv := patternView{
			Name: c.Name, Tag: c.Tag, Forming: c.Forming, OpenQuestion: c.OpenQuestion,
			CountLabel: fmt.Sprintf("%d items from %d sources", c.Count, c.Sources),
			Ideas:      ideaViews(c.Ideas),
		}
		for _, it := range c.Items {
			pv.Items = append(pv.Items, patternItem{Title: it.Title, URL: it.URL, Source: it.Source})
		}
		views = append(views, pv)
	}
	if f.GeneratedAt.IsZero() {
		return views, "No pattern pass yet. It runs at the end of a run once a week, or run scout patterns.", ""
	}
	if len(views) == 0 && f.Note == "" {
		f.Note = cluster.NoneNote
	}
	return views, f.Note, f.GeneratedAt.Local().Format("2 Jan, 15:04")
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

// BaseCSS is the digest's stylesheet with the font embedded, shared by other
// pages (the web console) so they look like the digest and need nothing from
// the network.
func BaseCSS() string { return fontFace() + css }

// fontFace embeds the Latin subset of Plus Jakarta Sans Variable (SIL OFL 1.1,
// see assets/FONT-LICENSE.txt) so the page needs nothing from the network.
func fontFace() string {
	var b strings.Builder
	b.WriteString(`@font-face{font-family:"Plus Jakarta Sans Variable";font-style:normal;font-display:swap;font-weight:200 800;src:url(data:font/woff2;base64,`)
	b.WriteString(base64.StdEncoding.EncodeToString(fontWOFF2))
	b.WriteString(`) format("woff2-variations");}` + "\n")
	return b.String()
}
