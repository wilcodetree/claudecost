// Package agg rolls parsed sessions up into month, week and day buckets,
// ported from the build() function of claude_usage_extract.py.
package agg

import (
	"fmt"
	"math"
	"sort"
	"time"

	"claudecost/internal/scan"
)

type SurfaceAgg struct {
	Calls    int64   `json:"calls"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
	CostSub  float64 `json:"cost_sub"`
	Sessions int64   `json:"sessions"`
}

type ModelAgg struct {
	Calls   int64   `json:"calls"`
	Tokens  int64   `json:"tokens"`
	Cost    float64 `json:"cost"`
	CostSub float64 `json:"cost_sub"`
}

// ToolAgg is one connector or plugin group's tool-call footprint inside a
// bucket. Keyed by scan.ToolGroup, not the raw tool name: per-tool detail
// stays on the session rows in the payload.
type ToolAgg struct {
	Calls    int64 `json:"calls"`
	Sessions int64 `json:"sessions"`
}

type Bucket struct {
	Calls     int64                  `json:"calls"`
	Tokens    int64                  `json:"tokens"`
	Cost      float64                `json:"cost"`
	CostSub   float64                `json:"cost_sub"`
	Sessions  int64                  `json:"sessions"`
	Fresh     int64                  `json:"fresh"`
	CacheW    int64                  `json:"cache_w"`
	CacheR    int64                  `json:"cache_r"`
	Out       int64                  `json:"out"`
	BySurface map[string]*SurfaceAgg `json:"by_surface"`
	ByModel   map[string]*ModelAgg   `json:"by_model"`
	ByTool    map[string]*ToolAgg    `json:"by_tool,omitempty"`
}

func newBucket() *Bucket {
	return &Bucket{
		BySurface: map[string]*SurfaceAgg{},
		ByModel:   map[string]*ModelAgg{},
		ByTool:    map[string]*ToolAgg{},
	}
}

func get(m map[string]*Bucket, k string) *Bucket {
	b := m[k]
	if b == nil {
		b = newBucket()
		m[k] = b
	}
	return b
}

func (b *Bucket) surf(k string) *SurfaceAgg {
	s := b.BySurface[k]
	if s == nil {
		s = &SurfaceAgg{}
		b.BySurface[k] = s
	}
	return s
}

func (b *Bucket) model(k string) *ModelAgg {
	m := b.ByModel[k]
	if m == nil {
		m = &ModelAgg{}
		b.ByModel[k] = m
	}
	return m
}

func (b *Bucket) tool(k string) *ToolAgg {
	t := b.ByTool[k]
	if t == nil {
		t = &ToolAgg{}
		b.ByTool[k] = t
	}
	return t
}

func monthKey(d time.Time) string {
	return fmt.Sprintf("%04d-%02d", d.Year(), int(d.Month()))
}

func weekKey(d time.Time) string {
	y, w := d.ISOWeek()
	return fmt.Sprintf("%04d-W%02d", y, w)
}

func round4(x float64) float64 {
	return math.Round(x*1e4) / 1e4
}

func parseDay(s string) (time.Time, bool) {
	d, err := time.Parse("2006-01-02", s)
	return d, err == nil
}

type target struct {
	key string
	m   map[string]*Bucket
}

// Build aggregates sessions starting on or after cutoff into month, week and
// day buckets, and returns the kept sessions newest first.
func Build(sessions []*scan.Session, cutoff time.Time) (months, weeks, days map[string]*Bucket, kept []*scan.Session) {
	months, weeks, days = map[string]*Bucket{}, map[string]*Bucket{}, map[string]*Bucket{}

	for _, s := range sessions {
		if len(s.Start) < 10 {
			continue
		}
		sd, ok := parseDay(s.Start[:10])
		if !ok || sd.Before(cutoff) {
			continue
		}
		kept = append(kept, s)

		// Session-level attribution goes to the day it started; the per-day
		// buckets below use each call's own date so a session that spans
		// midnight is split correctly.
		for _, t := range []target{{monthKey(sd), months}, {weekKey(sd), weeks}} {
			b := get(t.m, t.key)
			b.Sessions++
			b.surf(s.Surface).Sessions++
		}

		dayKeys := make([]string, 0, len(s.Daily))
		for k := range s.Daily {
			dayKeys = append(dayKeys, k)
		}
		sort.Strings(dayKeys)

		for _, daystr := range dayKeys {
			dv := s.Daily[daystr]
			dd, ok := parseDay(daystr)
			if !ok || dd.Before(cutoff) {
				continue
			}
			for _, t := range []target{{monthKey(dd), months}, {weekKey(dd), weeks}, {daystr, days}} {
				b := get(t.m, t.key)
				b.Calls += dv.Calls
				b.Tokens += dv.Tokens
				b.Cost += dv.Cost
				b.CostSub += dv.CostSub
				bs := b.surf(s.Surface)
				bs.Calls += dv.Calls
				bs.Tokens += dv.Tokens
				bs.Cost += dv.Cost
				bs.CostSub += dv.CostSub
			}
		}

		// Day buckets need a session count too, on the session's first day.
		firstDay := ""
		if len(dayKeys) > 0 {
			firstDay = dayKeys[0]
		}
		if firstDay != "" {
			if fd, ok := parseDay(firstDay); ok && !fd.Before(cutoff) {
				b := get(days, firstDay)
				b.Sessions++
				b.surf(s.Surface).Sessions++
			}
		}

		// Model split, whole-session granularity.
		mkeys := make([]string, 0, len(s.Models))
		for k := range s.Models {
			mkeys = append(mkeys, k)
		}
		sort.Strings(mkeys)
		targets := []target{{monthKey(sd), months}, {weekKey(sd), weeks}}
		if firstDay != "" {
			targets = append(targets, target{firstDay, days})
		}
		for _, t := range targets {
			b := get(t.m, t.key)
			for _, mn := range mkeys {
				mv := s.Models[mn]
				bm := b.model(mn)
				bm.Calls += mv.Calls
				bm.Tokens += mv.Tokens
				bm.Cost += mv.Cost
				bm.CostSub += mv.CostSub
			}
		}

		// Tool split, whole-session granularity, grouped by connector/plugin
		// (scan.ToolGroup) rather than raw tool name. Sessions is the count of
		// distinct sessions that used the group at least once, matching how
		// SurfaceAgg.Sessions is computed above; a session calling the same
		// group's tools several times still counts once.
		if len(s.Tools) > 0 {
			groupSeen := map[string]bool{}
			for tn := range s.Tools {
				groupSeen[scan.ToolGroup(tn)] = true
			}
			groups := make([]string, 0, len(groupSeen))
			for g := range groupSeen {
				groups = append(groups, g)
			}
			sort.Strings(groups)
			for _, t := range targets {
				b := get(t.m, t.key)
				for _, g := range groups {
					b.tool(g).Sessions++
				}
				for tn, calls := range s.Tools {
					b.tool(scan.ToolGroup(tn)).Calls += calls
				}
			}
		}

		// Token composition, attributed to the start month and week.
		for _, t := range []target{{monthKey(sd), months}, {weekKey(sd), weeks}} {
			b := get(t.m, t.key)
			b.Fresh += s.Fresh
			b.CacheW += s.CacheW
			b.CacheR += s.CacheR
			b.Out += s.Out
		}
	}

	sort.Slice(kept, func(i, j int) bool { return kept[i].Start > kept[j].Start })

	for _, m := range []map[string]*Bucket{months, weeks, days} {
		for _, b := range m {
			b.Cost = round4(b.Cost)
			b.CostSub = round4(b.CostSub)
			for _, s := range b.BySurface {
				s.Cost = round4(s.Cost)
				s.CostSub = round4(s.CostSub)
			}
			for _, mv := range b.ByModel {
				mv.Cost = round4(mv.Cost)
				mv.CostSub = round4(mv.CostSub)
			}
		}
	}
	return months, weeks, days, kept
}
