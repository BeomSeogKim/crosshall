package link

import (
	"testing"
	"time"
)

// The relay takes every value at once and passes them on in order.
func TestRelayNeverHoldsItsSenderBack(t *testing.T) {
	in, out := make(chan int), make(chan int)
	go Relay(t.Context(), in, out)
	for i := range 10000 {
		select {
		case in <- i:
		case <-time.After(5 * time.Second):
			t.Fatalf("value %d waited on the receiver", i)
		}
	}
	for i := range 10000 {
		if got := <-out; got != i {
			t.Fatalf("value %d came out as %d", i, got)
		}
	}
}
