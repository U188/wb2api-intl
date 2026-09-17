package upstream

import (
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestSanitizeRuntimeConcurrentToggle(t *testing.T) {
	c := New()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			c.SetSanitizeFingerprints(i%2 == 0)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			c.prepareBodySnapshot(auth.Snapshot{Site: auth.SiteCN}, []byte(`{"model":"glm-5.2","messages":[]}`))
		}
	}()
	wg.Wait()
	c.SetSanitizeFingerprints(false)
	if c.sanitizeEnabled() {
		t.Fatal("false runtime setting not applied")
	}
	c.SetSanitizeFingerprints(true)
	if !c.sanitizeEnabled() {
		t.Fatal("true runtime setting not applied")
	}
}
