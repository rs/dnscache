package dnscache

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestResolver_LookupHost(t *testing.T) {
	r := &Resolver{}
	var cacheMiss bool
	r.onCacheMiss = func() {
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
	r.LookupHost(context.Background(), "google.com")
	if e := r.cache["hgoogle.com"]; e == nil || !e.used {
		t.Error("cache entry used flag is false, want true")
	}
	r.Refresh(true)
	if e := r.cache["hgoogle.com"]; e == nil || e.used {
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

func TestHTTP(test *testing.T) {
	r := &Resolver{}
	t := &http.Transport{DialContext: r.Dial}
	c := &http.Client{Transport: t}
	res, err := c.Get("http://httpbin.org/status/418")
	if err == nil {
		if res.StatusCode != 418 {
			test.Errorf("expected 418, got: %d", res.StatusCode)
		}
	} else {
		test.Error(err.Error())
	}
	if e := r.cache["hhttpbin.org"]; e == nil || !e.used {
		test.Error("entry not cached")
	}
}
