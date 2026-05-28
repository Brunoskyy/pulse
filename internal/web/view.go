package web

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/Brunoskyy/pulse/internal/config"
)

// Page is everything the status page and the JSON API show.
type Page struct {
	Title     string        `json:"title"`
	Generated time.Time     `json:"generated_at"`
	Overall   string        `json:"overall"` // "operational", "degraded", "outage", "unknown"
	Headline  string        `json:"headline"`
	Groups    []Group       `json:"groups"`
	Incidents []IncidentRow `json:"incidents"`
}

type Group struct {
	Name   string     `json:"name"`
	Checks []CheckRow `json:"checks"`
}

type CheckRow struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Kind      string     `json:"kind"`
	State     string     `json:"state"` // "up", "down", "unknown"
	LastAt    *time.Time `json:"last_checked_at,omitempty"`
	LastError string     `json:"last_error,omitempty"`
	LatencyMS float64    `json:"latency_ms"`
	Uptime24h float64    `json:"uptime_24h"` // -1 when there is no data
	Uptime7d  float64    `json:"uptime_7d"`
	Uptime90d float64    `json:"uptime_90d"`
	P95MS24h  float64    `json:"p95_ms_24h"`
	Days      []DayCell  `json:"days"`
}

type DayCell struct {
	Date   string  `json:"date"`
	Uptime float64 `json:"uptime"` // -1 when there is no data
	Level  string  `json:"level"`  // "ok", "minor", "major", "none"
}

type IncidentRow struct {
	ID        int64      `json:"id"`
	CheckID   string     `json:"check_id"`
	CheckName string     `json:"check_name"`
	Reason    string     `json:"reason"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Ongoing   bool       `json:"ongoing"`
	Duration  string     `json:"duration"`
}

// Level classifies a day. 99.9% and up is a good day; 99% and up had a
// blip; anything below had a real outage.
func Level(uptime float64) string {
	switch {
	case uptime < 0:
		return "none"
	case uptime >= 0.999:
		return "ok"
	case uptime >= 0.99:
		return "minor"
	default:
		return "major"
	}
}

func (s *Server) build(ctx context.Context) (*Page, error) {
	now := s.Clock.Now()
	live, _ := s.Monitor.Snapshot()
	p := &Page{Title: s.Config.Title, Generated: now}
	byGroup := map[string]int{}
	down, known := 0, 0
	for _, c := range s.Config.Checks {
		row, err := s.row(ctx, c, now)
		if err != nil {
			return nil, err
		}
		if l, ok := live[c.ID]; ok && l.HasData {
			known++
			at := l.Last.At
			row.LastAt = &at
			row.LatencyMS = ms(l.Last.Latency)
			if l.Down {
				row.State = "down"
				row.LastError = l.Last.Error
				down++
			} else {
				row.State = "up"
				if !l.Last.OK {
					row.LastError = l.Last.Error
				}
			}
		}
		g := c.Group
		if g == "" {
			g = "Services"
		}
		i, ok := byGroup[g]
		if !ok {
			i = len(p.Groups)
			byGroup[g] = i
			p.Groups = append(p.Groups, Group{Name: g})
		}
		p.Groups[i].Checks = append(p.Groups[i].Checks, row)
	}
	switch {
	case known == 0:
		p.Overall, p.Headline = "unknown", "Waiting for the first checks"
	case down == 0:
		p.Overall, p.Headline = "operational", "All systems operational"
	case down == len(s.Config.Checks):
		p.Overall, p.Headline = "outage", "Every service is down"
	default:
		p.Overall = "degraded"
		p.Headline = fmt.Sprintf("%d of %d services down", down, len(s.Config.Checks))
		if down == 1 {
			p.Headline = "1 service down"
		}
	}

	names := map[string]string{}
	for _, c := range s.Config.Checks {
		names[c.ID] = c.Name
	}
	incidents, err := s.Store.Incidents(ctx, now.Add(-90*24*time.Hour), 25)
	if err != nil {
		return nil, err
	}
	for _, in := range incidents {
		name, ok := names[in.CheckID]
		if !ok {
			continue // a check that was removed from the config
		}
		p.Incidents = append(p.Incidents, IncidentRow{
			ID: in.ID, CheckID: in.CheckID, CheckName: name, Reason: in.Reason,
			StartedAt: in.StartedAt, EndedAt: in.EndedAt, Ongoing: in.EndedAt == nil,
			Duration: HumanDuration(in.Duration(now)),
		})
	}
	return p, nil
}

func (s *Server) row(ctx context.Context, c config.Check, now time.Time) (CheckRow, error) {
	row := CheckRow{ID: c.ID, Name: c.Name, Kind: string(c.Kind), State: "unknown", Uptime24h: -1, Uptime7d: -1, Uptime90d: -1}
	for _, w := range []struct {
		d   time.Duration
		dst *float64
	}{{24 * time.Hour, &row.Uptime24h}, {7 * 24 * time.Hour, &row.Uptime7d}, {90 * 24 * time.Hour, &row.Uptime90d}} {
		sum, err := s.Store.Summary(ctx, c.ID, now.Add(-w.d), now.Add(time.Millisecond), c.MaxGap)
		if err != nil {
			return row, err
		}
		*w.dst = sum.Uptime()
		if w.d == 24*time.Hour {
			row.P95MS24h = ms(sum.P95)
		}
	}
	days, err := s.Store.Days(ctx, c.ID, now.Add(-89*24*time.Hour), now.Add(time.Millisecond), c.MaxGap)
	if err != nil {
		return row, err
	}
	for _, d := range days {
		u := d.Uptime()
		row.Days = append(row.Days, DayCell{Date: d.Start.Format("2006-01-02"), Uptime: u, Level: Level(u)})
	}
	return row, nil
}

func ms(d time.Duration) float64 { return math.Round(float64(d.Microseconds())/10) / 100 }

// HumanDuration renders "3m", "1h 12m", "2d 4h".
func HumanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	d = d.Round(time.Minute)
	days := int(d / (24 * time.Hour))
	h := int(d/time.Hour) % 24
	m := int(d/time.Minute) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}
