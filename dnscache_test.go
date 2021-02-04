package dnscache

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolver_LookupHost(t *testing.T) {
	r := &Resolver{}
	var cacheMiss bool
	r.OnCacheMiss = func() {
		cacheMiss = true
	}
	hosts := []string{"google.com", "google.com.", "netflix.com"}
	for _, host := range hosts {
		t.Run(host, func(t *testing.T) {
			for _, wantMiss := range []bool{true, false, false} {
				cacheMiss = false
				addrs, err := r.LookupHost(context.Background(), host)
				if err != nil {
					t.Fatal(err)
				}
				if len(addrs) == 0 {
					t.Error("got no record")
				}
				for _, addr := range addrs {
					if net.ParseIP(addr) == nil {
						t.Errorf("got %q; want a literal IP address", addr)
					}
				}
				if wantMiss != cacheMiss {
					t.Errorf("got cache miss=%v, want %v", cacheMiss, wantMiss)
				}
			}
		})
	}
}

func TestClearCache(t *testing.T) {
	r := &Resolver{}
	_, _ = r.LookupHost(context.Background(), "google.com")
	if e := r.cache["hgoogle.com"]; e != nil && !e.used {
		t.Error("cache entry used flag is false, want true")
	}
	r.Refresh(true)
	if e := r.cache["hgoogle.com"]; e != nil && e.used {
		t.Error("cache entry used flag is true, want false")
	}
	r.Refresh(true)
	if e := r.cache["hgoogle.com"]; e != nil {
		t.Error("cache entry is not cleared")
	}

        options := ResolverRefreshOptions{}
        options.ClearUnused = true
        options.PersistOnFailure = false
	_, _ = r.LookupHost(context.Background(), "google.com")
	if e := r.cache["hgoogle.com"]; e != nil && !e.used {
		t.Error("cache entry used flag is false, want true")
	}
	r.RefreshWithOptions(options)
	if e := r.cache["hgoogle.com"]; e != nil && e.used {
		t.Error("cache entry used flag is true, want false")
	}
	r.RefreshWithOptions(options)
	if e := r.cache["hgoogle.com"]; e != nil {
		t.Error("cache entry is not cleared")
	}

        options.ClearUnused = false
        options.PersistOnFailure = true
        br := &Resolver{}
        br.Resolver = BadResolver{}

        _, _ = br.LookupHost(context.Background(), "google.com")
        br.Resolver = BadResolver{choke: true}
        br.RefreshWithOptions(options)
        if len(br.cache["hgoogle.com"].rrs) == 0 {
                t.Error("cache entry is cleared")
        }


}

func TestRaceOnDelete(t *testing.T) {
	r := &Resolver{}
	ls := make(chan bool)
	rs := make(chan bool)

	go func() {
		for {
			select {
			case <-ls:
				return
			default:
				r.LookupHost(context.Background(), "google.com")
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	go func() {
		for {
			select {
			case <-rs:
				return
			default:
				r.Refresh(true)
				time.Sleep(time.Millisecond)
			}
		}
	}()

	time.Sleep(1 * time.Second)

	ls <- true
	rs <- true

}

type fakeResolver struct {
	LookupHostCalls int32
	LookupAddrCalls int32
}

func (f *fakeResolver) LookupHost(ctx context.Context, host string) (addrs []string, err error) {
	atomic.AddInt32(&f.LookupHostCalls, 1)
	return nil, errors.New("not implemented")
}
func (f *fakeResolver) LookupAddr(ctx context.Context, addr string) (names []string, err error) {
	atomic.AddInt32(&f.LookupAddrCalls, 1)
	return nil, errors.New("not implemented")
}

func TestCacheFailTimeout(t *testing.T) {
	spy := fakeResolver{}
	r := &Resolver{
		CacheFailDuration: 10 * time.Millisecond,
		Resolver:          &spy,
	}
	_, err := r.LookupHost(context.Background(), "example.notexisting")
	if err == nil {
		t.Error("first lookup should have error")
	}
	initialCallCount := spy.LookupHostCalls
	if initialCallCount == 0 {
		t.Error("there should be a dns lookup")
	}

	_, err = r.LookupHost(context.Background(), "example.notexisting")
	if err == nil {
		t.Error("second lookup should have error")
	}

	if spy.LookupHostCalls != initialCallCount {
		t.Errorf("should have %d resolve calls, got %d", initialCallCount, spy.LookupHostCalls)
	}

	time.Sleep(10 * time.Millisecond)

	_, err = r.LookupHost(context.Background(), "example.notexisting")
	if err == nil {
		t.Error("post cache timeout lookup should have error")
	}
	if spy.LookupHostCalls <= initialCallCount {
		t.Errorf("should have more than %d calls", initialCallCount)
	}
}
