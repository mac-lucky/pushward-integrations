package syncx

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestPeriodic(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"runs fn at interval", func(t *testing.T) {
			var n atomic.Int32
			var p Periodic
			p.Start(context.Background(), 10*time.Millisecond, func(context.Context) {
				n.Add(1)
			})
			time.Sleep(55 * time.Millisecond)
			p.Stop()
			if got := n.Load(); got < 3 {
				t.Errorf("got %d ticks, want >=3", got)
			}
		}},
		{"Stop is idempotent", func(t *testing.T) {
			var p Periodic
			p.Start(context.Background(), time.Millisecond, func(context.Context) {})
			p.Stop()
			p.Stop()
			p.Stop()
		}},
		{"Stop before Start is safe", func(t *testing.T) {
			var p Periodic
			p.Stop()
		}},
		{"ctx cancel stops goroutine", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			var n atomic.Int32
			var p Periodic
			p.Start(ctx, 5*time.Millisecond, func(context.Context) {
				n.Add(1)
			})
			time.Sleep(20 * time.Millisecond)
			cancel()
			time.Sleep(20 * time.Millisecond)
			before := n.Load()
			time.Sleep(30 * time.Millisecond)
			if n.Load() != before {
				t.Errorf("goroutine kept running after ctx cancel")
			}
			p.Stop()
		}},
		{"Stop waits for in-flight fn", func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			var finished atomic.Bool
			var p Periodic
			p.Start(context.Background(), 5*time.Millisecond, func(context.Context) {
				select {
				case <-started:
				default:
					close(started)
				}
				<-release
				finished.Store(true)
			})
			<-started
			go func() {
				time.Sleep(20 * time.Millisecond)
				close(release)
			}()
			p.Stop()
			if !finished.Load() {
				t.Error("Stop returned before fn finished")
			}
		}},
		{"fn receives cancellable ctx", func(t *testing.T) {
			var p Periodic
			got := make(chan context.Context, 1)
			p.Start(context.Background(), 5*time.Millisecond, func(ctx context.Context) {
				select {
				case got <- ctx:
				default:
				}
			})
			ctx := <-got
			p.Stop()
			if ctx.Err() == nil {
				t.Error("expected ctx to be cancelled after Stop")
			}
		}},
		{"double Start panics", func(t *testing.T) {
			var p Periodic
			p.Start(context.Background(), 5*time.Millisecond, func(context.Context) {})
			defer p.Stop()
			defer func() {
				if recover() == nil {
					t.Error("expected panic on double Start")
				}
			}()
			p.Start(context.Background(), 5*time.Millisecond, func(context.Context) {})
		}},
		{"short interval high churn", func(t *testing.T) {
			var n atomic.Int32
			var p Periodic
			p.Start(context.Background(), time.Millisecond, func(context.Context) {
				n.Add(1)
			})
			time.Sleep(20 * time.Millisecond)
			p.Stop()
			if n.Load() == 0 {
				t.Error("expected at least one tick")
			}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, tc.run)
	}
}
