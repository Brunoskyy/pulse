package demo

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/Brunoskyy/pulse/internal/store"
)

func TestBackfillIsDeterministic(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	var totals [2]int
	for i := range 2 {
		st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
		if err != nil {
			t.Fatal(err)
		}
		if err := Backfill(context.Background(), st, DefaultServices(), now); err != nil {
			t.Fatal(err)
		}
		w, _ := st.Summary(context.Background(), "webhooks", now.Add(-91*24*time.Hour), now, 10*time.Minute)
		totals[i] = w.OK
		list, _ := st.Incidents(context.Background(), now.Add(-91*24*time.Hour), 50)
		if len(list) != len(history) {
			t.Fatalf("incidents %d, want %d", len(list), len(history))
		}
		st.Close()
	}
	if totals[0] != totals[1] {
		t.Fatalf("backfill differs between runs: %v", totals)
	}
}

func TestFleetToggles(t *testing.T) {
	f, err := Start([]*Service{{ID: "x", Name: "X", MinMS: 0, MaxMS: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	status := func() int {
		res, err := http.Get(f.Services[0].URL())
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if status() != 200 {
		t.Fatal("service should start up")
	}
	res, err := http.Post(f.Control+"/down/x", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if status() != 503 {
		t.Fatal("service should be down")
	}
}
