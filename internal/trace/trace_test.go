package trace

import (
	"context"
	"testing"
	"time"
)

func TestNestedStagesExcludeChildTime(t *testing.T) {
	ctx, r := New(context.Background())
	start := time.Now()
	parent, endParent := Start(ctx, "parent")
	_, endChild := Start(parent, "child")
	endChild()
	endParent()
	s := r.Stages()
	if len(s) != 2 || s[0].Name != "parent" || s[1].Name != "child" || s[0].Duration < 0 || s[1].Duration < 0 || s[0].Duration+s[1].Duration > time.Since(start) {
		t.Fatalf("bad exclusive stages: %+v", s)
	}
	s[0].Name = "mutated"
	if r.Stages()[0].Name != "parent" {
		t.Fatal("exposed mutable stages")
	}
}
