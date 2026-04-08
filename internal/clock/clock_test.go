package clock

import (
	"testing"
	"time"
)

func TestFakeFiresOnlyWhenDue(t *testing.T) {
	f := NewFake(time.Unix(0, 0))
	a := f.After(10 * time.Second)
	b := f.After(20 * time.Second)
	f.Advance(9 * time.Second)
	select {
	case <-a:
		t.Fatal("fired early")
	default:
	}
	f.Advance(time.Second)
	if got := <-a; !got.Equal(time.Unix(10, 0)) {
		t.Fatalf("fired at %v", got)
	}
	if f.Waiters() != 1 {
		t.Fatalf("waiters %d", f.Waiters())
	}
	f.Advance(time.Hour)
	<-b
	if c := f.After(0); len(c) != 1 {
		t.Fatal("a zero duration should fire at once")
	}
}
