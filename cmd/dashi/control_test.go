package main

import (
	"sync"
	"testing"
)

// Task wakes start work at any time, also while Wait waits and when no
// other work runs. That is not a race, and Wait still waits for all work.
func TestControllerWaitWhileWorkStarts(t *testing.T) {
	c := &Controller{}
	var starts sync.WaitGroup
	starts.Go(func() {
		for range 200 {
			c.goWork(func() {})
		}
	})
	for range 200 {
		c.Wait()
	}
	starts.Wait()
	c.Wait()
	if n := c.work.Load(); n != 0 {
		t.Fatalf("work=%d after Wait", n)
	}
}
