package server

import (
	"context"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestSiteWAFGatesAreIsolated(t *testing.T) {
	old := wafIPWindow
	wafIPWindow = time.Minute
	defer func() { wafIPWindow = old }()

	var gates siteWAFGates
	now := time.Now()
	cn := gates.forSite(auth.SiteCN)
	intl := gates.forSite(auth.SiteIntl)
	if cn.note("cn-1", now) {
		t.Fatal("one CN account must not trigger IP gate")
	}
	if cn.note("cn-2", now.Add(time.Second)) != true {
		t.Fatal("two distinct CN accounts should trigger CN gate")
	}
	if !cn.active(now.Add(2 * time.Second)) {
		t.Fatal("CN gate should be active")
	}
	if intl.active(now.Add(2 * time.Second)) {
		t.Fatal("CN WAF gate leaked into INTL")
	}
	if intl.note("intl-1", now.Add(2*time.Second)) {
		t.Fatal("one INTL account must not inherit CN hits")
	}
}

func TestWAFGateSameAccountDoesNotTrigger(t *testing.T) {
	var g wafIPGate
	now := time.Now()
	if g.note("u1", now) || g.note("u1", now.Add(time.Second)) {
		t.Fatal("same account repeated WAF must not be treated as multi-account IP block")
	}
}

func TestBackoffBoundsAndCancellation(t *testing.T) {
	old := rotateBackoffBase
	rotateBackoffBase = 100 * time.Millisecond
	defer func() { rotateBackoffBase = old }()
	for i := 0; i < 10; i++ {
		d := backoffAfter(i)
		base := 100 * time.Millisecond
		for n := 0; n < i && base < rotateBackoffCap; n++ {
			base *= 2
			if base > rotateBackoffCap {
				base = rotateBackoffCap
			}
		}
		min, max := time.Duration(float64(base)*0.75), time.Duration(float64(base)*1.25)
		if d < min || d > max {
			t.Fatalf("attempt %d duration %v outside [%v,%v]", i, d, min, max)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Second) {
		t.Fatal("cancelled context should abort backoff")
	}
}
