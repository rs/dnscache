package dnscache

import (
	"context"
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

func TestCacheFailTimeout(t *testing.T) {
	resolveCalls := int32(0)
	mainResolver := net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			atomic.AddInt32(&resolveCalls, 1)
			return net.Dial(network, address)
		},
	}
	r := &Resolver{
		CacheFailDuration: 10 * time.Millisecond,
		Resolver:          &mainResolver,
	}
	_, err := r.LookupHost(context.Background(), "example.notexisting")
	if err == nil {
		t.Error("first lookup should have error")
	}
	initialCallCount := resolveCalls

	_, err = r.LookupHost(context.Background(), "example.notexisting")
	if err == nil {
		t.Error("second lookup should have error")
	}

	if resolveCalls != initialCallCount {
		t.Errorf("should have %d resolve calls, got %d", initialCallCount, resolveCalls)
	}

	time.Sleep(10 * time.Millisecond)

	_, err = r.LookupHost(context.Background(), "example.notexisting")
	if err == nil {
		t.Error("post cache timeout lookup should have error")
	}
	if resolveCalls <= initialCallCount {
		t.Errorf("should have more than %d calls", initialCallCount)
	}
}
