package request

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

func TestHeartbeatUntilCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var beats int
		done := make(chan struct{})
		go func() {
			defer close(done)
			heartbeatUntilCancelled(ctx, func() { beats++ })
		}()
		synctest.Wait()

		time.Sleep(31 * time.Second)
		synctest.Wait()
		if beats != 3 {
			t.Fatalf("heartbeats = %d, want 3", beats)
		}
		cancel()
		<-done
		time.Sleep(20 * time.Second)
		if beats != 3 {
			t.Fatalf("heartbeats after cancellation = %d, want 3", beats)
		}
	})
}
