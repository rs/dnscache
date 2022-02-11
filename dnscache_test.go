package dnscache

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
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
	if e, found := r.cache.Get("hgoogle.com"); found != false {
		entry, ok := e.(*cacheEntry)
		if !ok || !entry.used {
			t.Error("cache entry used flag is false, want true, used:", entry.used)
		}
	}
	// 在刷新周期里，使用过，这里拿到了缓存
	r.Refresh(true)
	if e, found := r.cache.Get("hgoogle.com"); found != false {
		entry, ok := e.(*cacheEntry)
		if !ok || !entry.used {
			t.Error("cache entry used flag is false, want true, used:", entry.used)
		}
	}
	// 在刷新间隔里未使用，把未使用的移除了，所以拿不到缓存
	r.Refresh(true)
	if e, found := r.cache.Get("hgoogle.com"); found != false {
		entry, ok := e.(*cacheEntry)
		if ok {
			t.Error("cache entry is not cleared, used:", entry.used)
		}
	}

	options := ResolverRefreshOptions{}
	options.ClearUnused = true
	options.PersistOnFailure = false
	_, _ = r.LookupHost(context.Background(), "google.com")
	if e, found := r.cache.Get("hgoogle.com"); found != false {
		entry, ok := e.(*cacheEntry)
		if ok {
			t.Error("cache entry used flag is false, want true, used:", entry.used)
		}
	}
	r.RefreshWithOptions(options)
	if e, found := r.cache.Get("hgoogle.com"); found != false {
		entry, ok := e.(*cacheEntry)
		if ok {
			t.Error("cache entry used flag is true, want false, used:", entry.used)
		}
	}
	r.RefreshWithOptions(options)
	if e, found := r.cache.Get("hgoogle.com"); found != false {
		entry, ok := e.(*cacheEntry)
		if ok {
			t.Error("ccache entry is not cleared, used:", entry.used)
		}
	}

	options.ClearUnused = false
	options.PersistOnFailure = true
	br := &Resolver{}
	br.Resolver = BadResolver{}

	_, _ = br.LookupHost(context.Background(), "google.com")
	br.Resolver = BadResolver{choke: true}
	br.RefreshWithOptions(options)
	if e, found := br.cache.Get("hgoogle.com"); found != false {
		if entry, ok := e.(*cacheEntry); ok != false {
			if len(entry.rrs) == 0 {
				t.Error("cache entry is cleared")
			}
		}
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
	if e, found := r.cache.Get("hgoogle.com"); found != false {
		entry, ok := e.(*cacheEntry)
		if !ok {
			log.Printf("xnnnjjdjdj, entry:%v, ok:%v", entry, ok)
			//t.Error("cache entry used flag is false, want true, used:", entry.used)
		}
		if entry.used {
			t.Logf("cache entry, rrs:%v expire:%v", entry.rrs, time.Unix(entry.expire, 0).Format(timeTemplate1))
		}
	}
	_, _ = r.LookupHost(context.Background(), "google.com")

	if e, found := r.cache.Get("hgoogle.com"); found != false {
		entry, ok := e.(*cacheEntry)
		if !ok {
			t.Error("cache entry used flag is false, want true, used:", entry.used)
		}
		if entry.used {
			t.Logf("cache entry, rrs:%v expire:%v", entry.rrs, time.Unix(entry.expire, 0).Format(timeTemplate1))
		}
	}

	_, _ = r.LookupHost(context.Background(), "baidu.com")
	if e, found := r.cache.Get("hbaidu.com"); found != false {
		entry, ok := e.(*cacheEntry)
		if !ok {
			t.Error("cache entry used flag is false, want true, used:", entry.used)
		}
		if entry.used {
			t.Logf("cache entry, rrs:%v expire:%v", entry.rrs, time.Unix(entry.expire, 0).Format(timeTemplate1))
		}
	}

	time.Sleep(1 * time.Minute)
	_, _ = r.LookupHost(context.Background(), "google.com")
	if e, found := r.cache.Get("hgoogle.com"); found != false {
		entry, ok := e.(*cacheEntry)
		if !ok {
			t.Error("cache entry used flag is false, want true, used:", entry.used)
		}
		if entry.used {
			t.Logf("cache entry, rrs:%v expire:%v", entry.rrs, time.Unix(entry.expire, 0).Format(timeTemplate1))
		}
	}

	_, _ = r.LookupHost(context.Background(), "baidu.com")
	if e, found := r.cache.Get("hbaidu.com"); found != false {
		entry, ok := e.(*cacheEntry)
		if !ok {
			t.Error("cache entry used flag is false, want true, used: ", entry.used)
		}
		if entry.used {
			t.Logf("cache entry, rrs:%v expire:%v", entry.rrs, time.Unix(entry.expire, 0).Format(timeTemplate1))
		}
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
//func Benchmark_cacheMap(b *testing.B) {
//	r := &Resolver{}
//	r.init()
//	m := r.cache
//	for i := 0; i < b.N; i++ {
//		func() {
//			m[string(i)] = &cacheEntry{
//				rrs:    []string{"983493848", ""},
//				err:    nil,
//				used:   false,
//				expire: 0,
//			}
//
//		}()
//	}
//	for i := 0; i < b.N; i++ {
//		func() {
//			_ = m[string(i)]
//		}()
//	}
//}

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

	res, err := c.Get("http://www.baidu.com")
	if err == nil {
		fmt.Printf("StatusCode：%v", res.StatusCode)
	}
}

func TestHitCache(t *testing.T) {
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

	res, err := c.Get("http://www.baidu.com")
	if err == nil {
		fmt.Printf("StatusCode：%v", res.StatusCode)
	}
	res2, err := c.Get("http://www.163.com")
	if err == nil {
		fmt.Printf("StatusCode：%v", res2.StatusCode)
	}
	time.Sleep(5)
	res1, err := c.Get("http://www.baidu.com")
	if err == nil {
		fmt.Printf("StatusCode：%v", res1.StatusCode)
	}
}

func TestHitCache1(t *testing.T) {
	fmt.Println("嗨客网(www.haicoder.net)")
	// 使用接口类型转换，将接口类型转成指针类型
	var svalue interface{}
	str := []string{"HaiCoder"}
	svalue = &str
	if value, ok := svalue.(*string); ok {
		fmt.Println("Ok Value =", *value, "Ok =", ok)
	} else {
		fmt.Println("Failed Value =", value, "Ok =", ok)
	}
}

/**
go test  -v -race  -run=TestLoad
*/
func BenchmarkTestLoad(b *testing.B) {
	r, _ := New(3*time.Second, 5*time.Second, 1*time.Minute, &ResolverRefreshOptions{
		ClearUnused:       false,
		PersistOnFailure:  false,
		CacheExpireUnused: true,
	})
	_, _ = r.LookupHost(context.Background(), "google.com")
	key := "hgoogle.com"
	for i := 0; i < b.N; i++ {
		_, _, _ = r.load(key)
	}
}

/**
将一万个不重复的键值对同时以200万次写和200万次读
➜  dnscache git:(ConcurrentMapShared) ✗ go test -run=none -bench=BenchmarkMapShared -benchmem .
goos: darwin
goarch: amd64
pkg: github.com/monicapu/dnscache
BenchmarkMapShared-12            2113465               570 ns/op              64 B/op          1 allocs/op
PASS
ok      github.com/monicapu/dnscache    2.070s

*/
func BenchmarkMapShared(b *testing.B) {
	r, _ := New(3*time.Second, 5*time.Second, 1*time.Minute, &ResolverRefreshOptions{
		ClearUnused:       false,
		PersistOnFailure:  false,
		CacheExpireUnused: true,
	})
	r.init()
	num := 10000
	testCase := genNoRepeatTestCase(num) // 10000个不重复的键值对
	for _, v := range testCase {
		r.storeLocked(v.Key, v.Val, true, nil)
	}
	b.ResetTimer()

	wg := sync.WaitGroup{}
	wg.Add(b.N * 2)
	for i := 0; i < b.N; i++ {
		e := testCase[rand.Intn(num)]
		go func(key string, val []string) {
			r.storeLocked(key, val, true, nil)
			wg.Done()
		}(e.Key, e.Val)

		go func(key string) {
			_, _, _ = r.load(key)
			wg.Done()
		}(e.Key)
	}
	wg.Wait()
}

type testRam struct {
	Key string
	Val []string
}

func genNoRepeatTestCase(num int) []testRam {
	var res []testRam
	for i := 0; i < num; i++ {
		res = append(res, testRam{
			Key: "hgoogle " + strconv.Itoa(i),
			Val: []string{"hgoogleaddr" + strconv.Itoa(i)},
		})
	}
	return res
}
