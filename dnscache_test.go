package dnscache

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"testing"
	"time"
)

var (
	testFreq                 = 1 * time.Second
	testDefaultLookupTimeout = 1 * time.Second
	testCacheTimeout         = 1 * time.Minute
)

func testResolver(t *testing.T) *Resolver {
	t.Helper()
	r, err := New(testFreq, testDefaultLookupTimeout, testCacheTimeout, nil)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	return r
}

func TestNew(t *testing.T) {
	if _, err := New(testFreq, testDefaultLookupTimeout, testCacheTimeout, nil); err != nil {
		t.Fatalf("expect not to be failed")
	}

	{
		resolver, err := New(testFreq, testDefaultLookupTimeout, testCacheTimeout, nil)
		if err != nil {
			t.Fatalf("expect not to be failed")
		}
		resolver.Stop()
	}

	{
		resolver, err := New(0, 0, 0, nil)
		if err != nil {
			t.Fatalf("expect not to be failed")
		}
		resolver.Stop()
	}
}

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

func TestResolver_LookupHost_DNSHooksGetTriggerd(t *testing.T) {
	var (
		dnsStartInfo *httptrace.DNSStartInfo
		dnsDoneInfo  *httptrace.DNSDoneInfo
	)

	trace := &httptrace.ClientTrace{
		DNSStart: func(info httptrace.DNSStartInfo) {
			dnsStartInfo = &info
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			dnsDoneInfo = &info
		},
	}

	ctx := httptrace.WithClientTrace(context.Background(), trace)

	r := &Resolver{}

	_, err := r.LookupHost(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}

	if dnsStartInfo == nil {
		t.Error("dnsStartInfo is nil, indicating that DNSStart callback has not been invoked")
	}

	if dnsDoneInfo == nil {
		t.Error("dnsDoneInfo is nil, indicating that DNSDone callback has not been invoked")
	}
}

/**
go test  -v -race  -run=TestConcurrentExample
*/
func TestConcurrentExample(t *testing.T) {
	resolver, _ := New(3*time.Second, 5*time.Second, 10*time.Minute, &ResolverRefreshOptions{
		ClearUnused:       false,
		PersistOnFailure:  false,
		CacheExpireUnused: true,
	})
	// You can create a HTTP client which selects an IP from dnscache
	// randomly and dials it.
	rand.Seed(time.Now().UTC().UnixNano()) // You MUST run in once in your application

	transport := &http.Transport{
		DialContext: DialFunc(resolver, nil),
	}
	c := &http.Client{Transport: transport}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := c.Get("http://www.baidu.com")
			if err == nil {
				fmt.Printf("StatusCode：%v", res.StatusCode)
			}
		}()
	}
	wg.Wait()
}

func TestCacheTimeout(t *testing.T) {
	timeTemplate1 := "2006-01-02 15:04:05" //常规类型

	r, _ := New(3*time.Second, 5*time.Second, 1*time.Minute, &ResolverRefreshOptions{
		ClearUnused:       false,
		PersistOnFailure:  false,
		CacheExpireUnused: true,
	})
	_, _ = r.LookupHost(context.Background(), "google.com")
	if e := r.cache["hgoogle.com"]; e != nil && e.used {
		t.Logf("cache entry, rrs:%v expire:%v", e.rrs, time.Unix(e.expire, 0).Format(timeTemplate1))
	}
	_, _ = r.LookupHost(context.Background(), "baidu.com")
	if e := r.cache["hbaidu.com"]; e != nil && e.used {
		t.Logf("cache entry, rrs:%v expire:%v", e.rrs, time.Unix(e.expire, 0).Format(timeTemplate1))
	}
	time.Sleep(1 * time.Minute)
	_, _ = r.LookupHost(context.Background(), "google.com")
	if e := r.cache["hgoogle.com"]; e != nil {
		t.Logf("cache entry, rrs:%v expire:%v", e.rrs, time.Unix(e.expire, 0).Format(timeTemplate1))
	}
	_, _ = r.LookupHost(context.Background(), "baidu.com")
	if e := r.cache["hbaidu.com"]; e != nil && e.used {
		t.Logf("cache entry, rrs:%v expire:%v", e.rrs, time.Unix(e.expire, 0).Format(timeTemplate1))
	}
}

func TestTime(t *testing.T) {
	timeTemplate1 := "2006-01-02 15:04:05"
	t1 := time.Now().Unix()
	log.Println(time.Unix(t1, 0).Format(timeTemplate1))
}

func TestCacheMap(t *testing.T) {
	r := &Resolver{}
	m := r.cache
	log.Printf("%v", m)

}

/**
➜  dnscache git:(cache_map_add_expire) ✗ go test -run=none -bench=Benchmark_cacheMap -benchmem .
goos: darwin
goarch: amd64
pkg: github.com/rs/dnscache
Benchmark_cacheMap-12            3469770               301 ns/op             135 B/op          3 allocs/op
PASS
ok      github.com/rs/dnscache  3.837s
*/
func Benchmark_cacheMap(b *testing.B) {
	r := &Resolver{}
	r.init()
	m := r.cache
	for i := 0; i < b.N; i++ {
		func() {
			m[string(i)] = &cacheEntry{
				rrs:    []string{"983493848", ""},
				err:    nil,
				used:   false,
				expire: 0,
			}

		}()
	}
	for i := 0; i < b.N; i++ {
		func() {
			_ = m[string(i)]
		}()
	}
}

func TestExample(t *testing.T) {
	resolver, _ := New(3*time.Second, 5*time.Second, 10*time.Minute, &ResolverRefreshOptions{
		ClearUnused:       false,
		PersistOnFailure:  false,
		CacheExpireUnused: true,
	})
	// You can create a HTTP client which selects an IP from dnscache
	// randomly and dials it.
	rand.Seed(time.Now().UTC().UnixNano()) // You MUST run in once in your application

	transport := &http.Transport{
		DialContext: DialFunc(resolver, nil),
	}
	c := &http.Client{Transport: transport}

	go func() {
		res, err := c.Get("http://www.baidu.com")
		if err == nil {
			fmt.Printf("StatusCode：%v", res.StatusCode)
		}
	}()
}
