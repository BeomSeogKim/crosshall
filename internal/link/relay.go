package link

import "context"

// Relay passes values from in to out in order and is always ready to take
// the next from in. A connection's reader sends what it reads through it,
// so reading never waits on the loop that consumes the frames, and a loop
// blocked writing to a peer that writes before it reads cannot stall the
// connection (#400).
func Relay[T any](ctx context.Context, in <-chan T, out chan<- T) {
	var queue []T
	for {
		var send chan<- T
		var head T
		if len(queue) > 0 {
			send, head = out, queue[0]
		}
		select {
		case v := <-in:
			queue = append(queue, v)
		case send <- head:
			queue = queue[1:]
		case <-ctx.Done():
			return
		}
	}
}
