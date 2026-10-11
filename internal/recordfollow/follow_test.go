package recordfollow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

var (
	testAhead   = errors.New("ahead")
	testInvalid = errors.New("invalid")
)

type readFunc func(context.Context, string, int64, int) ([]Position, error)

func (f readFunc) StepsAfter(
	ctx context.Context, id string, after int64, limit int,
) ([]Position, error) {
	return f(ctx, id, after, limit)
}

type waitingReader struct {
	readFunc
	wait func(context.Context, string, int64) error
}

func (r waitingReader) Wait(ctx context.Context, id string, after int64) error {
	return r.wait(ctx, id, after)
}

func position(p Position) Position {
	return p
}

func TestFollowDrainsPagesPastEndWithoutWaiting(t *testing.T) {
	for _, after := range []int64{-1, 3, 7} {
		reads := 0
		r := waitingReader{
			readFunc: func(_ context.Context, id string, from int64, limit int) ([]Position, error) {
				reads++
				var page []Position
				for seq := from + 1; seq <= 208 && len(page) < limit; seq++ {
					page = append(page, Position{
						TurnID: id, Seq: seq, Ended: seq == 3, Resting: seq == 3,
					})
				}
				return page, nil
			},
			wait: func(context.Context, string, int64) error {
				t.Fatal("reader waited before draining available pages")
				return nil
			},
		}
		cursor := max(after, 0)
		for step, err := range Follow(
			t.Context(), r, "t", after, position, false,
			testAhead, testInvalid, Defaults(),
		) {
			if err != nil {
				t.Fatal(err)
			}
			cursor++
			if step.Seq != cursor {
				t.Fatalf("received seq %d; want %d", step.Seq, cursor)
			}
			if step.Seq == 208 {
				break
			}
		}
		if cursor != 208 || reads != 3 {
			t.Fatalf("cursor %d, page reads %d", cursor, reads)
		}
	}
}

func TestFollowBoundsMissedWaiterNotifications(t *testing.T) {
	reads := 0
	r := waitingReader{
		readFunc: func(context.Context, string, int64, int) ([]Position, error) {
			reads++
			if reads == 3 {
				return []Position{{TurnID: "t", Seq: 1}}, nil
			}
			return nil, nil
		},
		wait: func(ctx context.Context, _ string, _ int64) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	bounds := Defaults()
	bounds.Backstop = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	seen := false
	for _, err := range Follow(
		ctx, r, "t", 0, position, false, testAhead, testInvalid, bounds,
	) {
		if err != nil {
			t.Fatal(err)
		}
		seen = true
		break
	}
	if !seen {
		t.Fatal("missed notifications prevented record delivery")
	}
}

func TestFollowBacksOffFailedAndEagerWaiters(t *testing.T) {
	for _, failing := range []bool{false, true} {
		reads := 0
		r := waitingReader{
			readFunc: func(context.Context, string, int64, int) ([]Position, error) {
				reads++
				return nil, nil
			},
			wait: func(context.Context, string, int64) error {
				if failing {
					return errors.New("hint unavailable")
				}
				return nil
			},
		}
		bounds := Defaults()
		bounds.Poll, bounds.Floor = 10*time.Millisecond, 10*time.Millisecond
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
		for _, err := range Follow(
			ctx, r, "t", 0, position, false, testAhead, testInvalid, bounds,
		) {
			t.Fatalf("empty reader yielded an event: %v", err)
		}
		cancel()
		if reads > 10 {
			t.Fatalf("failing=%t made %d reads; waiter caused a spin", failing, reads)
		}
	}
}

func TestFollowCancellationFinishesReadAndWait(t *testing.T) {
	for _, waiting := range []bool{false, true} {
		var active atomic.Int64
		entered := make(chan struct{})
		block := func(ctx context.Context) error {
			active.Add(1)
			defer active.Add(-1)
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		}
		r := waitingReader{
			readFunc: func(ctx context.Context, _ string, _ int64, _ int) ([]Position, error) {
				if waiting {
					return nil, nil
				}
				return nil, block(ctx)
			},
			wait: func(ctx context.Context, _ string, _ int64) error {
				return block(ctx)
			},
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			for _, err := range Follow(
				ctx, r, "t", 0, position, false,
				testAhead, testInvalid, Defaults(),
			) {
				if err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("reader did not enter its blocking operation")
		}
		cancel()
		select {
		case err := <-done:
			if err != nil || active.Load() != 0 {
				t.Fatalf("reader work remained after cancellation: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("cancellation did not finish the reader")
		}
	}
}

func TestFollowValidatesTheCursorRow(t *testing.T) {
	for _, tc := range []struct {
		name string
		page []Position
		want error
	}{
		{"missing cursor", nil, testAhead},
		{"wrong cursor turn", []Position{{TurnID: "other", Seq: 4}}, testInvalid},
		{"wrong cursor sequence", []Position{{TurnID: "t", Seq: 5}}, testInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := readFunc(func(context.Context, string, int64, int) ([]Position, error) {
				return tc.page, nil
			})
			count := 0
			for _, err := range Follow(
				t.Context(), r, "t", 4, position, false,
				testAhead, testInvalid, Defaults(),
			) {
				count++
				if !errors.Is(err, tc.want) {
					t.Fatalf("cursor error = %v; want %v", err, tc.want)
				}
			}
			if count != 1 {
				t.Fatalf("cursor yielded %d errors; want one", count)
			}
		})
	}
}
