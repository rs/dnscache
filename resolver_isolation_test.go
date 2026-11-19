package dnscache_test

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/dnscache"
)

type isolationResolver struct {
	started chan struct{}
	release chan struct{}
	result  []string
	err     error
	calls   int32
}

func (r *isolationResolver) lookup(ctx context.Context) ([]string, error) {
	atomic.AddInt32(&r.calls, 1)
	if r.started != nil {
		r.started <- struct{}{}
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return r.result, r.err
}

func (r *isolationResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	return r.lookup(ctx)
}

func (r *isolationResolver) LookupAddr(ctx context.Context, addr string) ([]string, error) {
	return r.lookup(ctx)
}

func TestResolverIndependentLookups(t *testing.T) {
	backendError := errors.New("first backend unavailable")
	for _, reverse := range []bool{false, true} {
		for _, firstError := range []error{nil, backendError} {
			name := "host"
			if reverse {
				name = "addr"
			}
			if firstError != nil {
				name += "/error"
			}
			t.Run(name, func(t *testing.T) {
				first := &isolationResolver{
					started: make(chan struct{}, 1),
					release: make(chan struct{}),
					result:  []string{"192.0.2.1"},
					err:     firstError,
				}
				second := &isolationResolver{result: []string{"192.0.2.2"}}
				if reverse {
					first.result = []string{"first.example"}
					second.result = []string{"second.example"}
				}
				a := &dnscache.Resolver{Resolver: first}
				b := &dnscache.Resolver{Resolver: second}
				lookup := func(ctx context.Context, r *dnscache.Resolver) ([]string, error) {
					if reverse {
						return r.LookupAddr(ctx, "192.0.2.10")
					}
					return r.LookupHost(ctx, "service.example")
				}
				firstDone := make(chan struct{})
				var firstResult []string
				var gotFirstError error
				go func() {
					defer close(firstDone)
					firstResult, gotFirstError = lookup(context.Background(), a)
				}()
				defer func() {
					close(first.release)
					<-firstDone
					if gotFirstError != firstError {
						t.Errorf("first lookup error = %v, want %v", gotFirstError, firstError)
					}
					if firstError == nil && !reflect.DeepEqual(firstResult, first.result) {
						t.Errorf("first lookup = %v, want %v", firstResult, first.result)
					}
				}()
				<-first.started

				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				for i := 0; i < 2; i++ {
					got, err := lookup(ctx, b)
					if err != nil || !reflect.DeepEqual(got, second.result) {
						t.Fatalf("second lookup = %v, %v; want %v, nil while first backend is blocked", got, err, second.result)
					}
				}
				if calls := atomic.LoadInt32(&second.calls); calls != 1 {
					t.Errorf("second backend called %d times, want 1 (cached)", calls)
				}
			})
		}
	}
}

func TestResolverSharedLookupCanceledWaiter(t *testing.T) {
	backend := &isolationResolver{
		started: make(chan struct{}, 2),
		release: make(chan struct{}),
		result:  []string{"192.0.2.3"},
	}
	r := &dnscache.Resolver{Resolver: backend}
	done := make(chan struct{})
	go func() {
		defer close(done)
		got, err := r.LookupHost(context.Background(), "shared.example")
		if err != nil || !reflect.DeepEqual(got, backend.result) {
			t.Errorf("first lookup = %v, %v; want %v, nil", got, err, backend.result)
		}
	}()
	defer func() {
		close(backend.release)
		<-done
	}()
	<-backend.started

	ctx, cancel := context.WithCancel(context.Background())
	// A canceled waiter must leave the shared lookup running for the first
	// caller, without starting another backend request.
	cancel()
	_, err := r.LookupHost(ctx, "shared.example")
	if err != context.Canceled {
		t.Fatalf("canceled waiter error = %v, want context.Canceled", err)
	}
	if calls := atomic.LoadInt32(&backend.calls); calls != 1 {
		t.Fatalf("backend called %d times, want one shared lookup", calls)
	}
}
