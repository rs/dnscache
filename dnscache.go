package dnscache

import (
	"context"
	"log"
	"net"
	"net/http/httptrace"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

var (
	defaultFreq          = 3 * time.Second
	defaultLookupTimeout = 10 * time.Second
	DefaultCacheTimeout  = 10 * time.Minute
)

type DNSResolver interface {
	LookupHost(ctx context.Context, host string) (addrs []string, err error)
	LookupAddr(ctx context.Context, addr string) (names []string, err error)
}

type Resolver struct {
	// Timeout defines the maximum allowed time allowed for a lookup.
	Timeout time.Duration

	// Resolver is used to perform actual DNS lookup. If nil,
	// net.DefaultResolver is used instead.
	Resolver DNSResolver

	once  sync.Once
	mu    sync.RWMutex
	cache map[string]*cacheEntry

	// OnCacheMiss is executed if the host or address is not included in
	// the cache and the default lookup is executed.
	OnCacheMiss func()

	// cache timeout, when cache will expire, then refresh the key
	CacheTimeout time.Duration
	// 缓存到期时间需要大于2倍刷新时间
	// 自动刷新时间间隔
	RefreshTime time.Duration

	closer func()
}

type ResolverRefreshOptions struct {
	ClearUnused      bool
	PersistOnFailure bool
	// ClearUnused 方案是，在上一个刷新时间周期里若缓存没有被访问则删除
	// ClearUnused 方案，在每次访问缓存时需要加读锁和写锁，性能不太好
	// 如果采用 CacheExpireUnused 缓存过期方案，ClearUnused 策略便不使用了
	CacheExpireUnused bool
}

type cacheEntry struct {
	rrs    []string
	err    error
	used   bool
	expire int64 //刷新的时候赋值，当前时间+过期时间
}

// New initializes DNS cache resolver and starts auto refreshing in a new goroutine.
// To stop refreshing, call `Stop()` function.
func New(freq time.Duration, lookupTimeout time.Duration, cacheTimeout time.Duration, options *ResolverRefreshOptions) (*Resolver, error) {
	if freq <= 0 {
		freq = defaultFreq
	}

	if lookupTimeout <= 0 {
		lookupTimeout = defaultLookupTimeout
	}

	if cacheTimeout <= 0 {
		cacheTimeout = DefaultCacheTimeout
	}
	if options == nil {
		options = &ResolverRefreshOptions{
			ClearUnused:       false,
			PersistOnFailure:  false,
			CacheExpireUnused: true,
		}
	}

	ticker := time.NewTicker(freq)
	ch := make(chan struct{})
	closer := func() {
		ticker.Stop()
		close(ch)
	}

	// copy handler function to avoid race
	//onRefreshedFn := onRefreshed
	//lookupIPFn := lookupIP

	r := &Resolver{
		Timeout:      lookupTimeout,
		CacheTimeout: cacheTimeout,
		RefreshTime:  freq,
		closer:       closer,
	}

	go func() {
		for {
			select {
			case <-ticker.C:
				log.Print("dnscache refresh ticker")
				r.RefreshWithOptions(*options)
				//onRefreshedFn()
			case <-ch:
				return
			}
		}
	}()

	return r, nil
}

// Stop stops auto refreshing.
func (r *Resolver) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closer != nil {
		r.closer()
		r.closer = nil
	}
}

// LookupAddr performs a reverse lookup for the given address, returning a list
// of names mapping to that address.
func (r *Resolver) LookupAddr(ctx context.Context, addr string) (names []string, err error) {
	r.once.Do(r.init)
	return r.lookup(ctx, "r"+addr)
}

// LookupHost looks up the given host using the local resolver. It returns a
// slice of that host's addresses.
func (r *Resolver) LookupHost(ctx context.Context, host string) (addrs []string, err error) {
	r.once.Do(r.init)
	return r.lookup(ctx, "h"+host)
}

// refreshRecords refreshes cached entries which have been used at least once since
// the last Refresh. If clearUnused is true, entries which haven't be used since the
// last Refresh are removed from the cache. If persistOnFailure is true, stale
// entries will not be removed on failed lookups
func (r *Resolver) refreshRecords(clearUnused bool, persistOnFailure bool, cacheExpireUnused bool) {
	r.once.Do(r.init)
	if cacheExpireUnused {
		r.refreshRecordsByCacheTimeout(persistOnFailure, cacheExpireUnused)
		return
	}
	r.mu.RLock()
	update := make([]string, 0, len(r.cache))
	del := make([]string, 0, len(r.cache))
	for key, entry := range r.cache {
		if entry.used {
			update = append(update, key)
		} else if clearUnused {
			del = append(del, key)
		}
	}
	r.mu.RUnlock()

	if len(del) > 0 {
		r.mu.Lock()
		for _, key := range del {
			delete(r.cache, key)
		}
		r.mu.Unlock()
	}

	for _, key := range update {
		// todo ,待测试打印，循环和日志打印地方
		rrs, err := r.update(context.Background(), key, false, persistOnFailure)
		if err != nil {
			log.Printf("update dnscache has some error, key: %v, rrs: %v, err:%v", key, rrs, err)
		}
	}
}

func (r *Resolver) refreshRecordsByCacheTimeout(persistOnFailure bool, cacheExpireUnused bool) {
	r.mu.RLock()
	update := make([]string, 0, len(r.cache))
	for key, entry := range r.cache {
		// 距离缓存到期多久前，需要触发刷新动作：缓存到期时间需要大于2倍刷新时间
		if (entry.expire - time.Now().Unix()) <= r.RefreshTime.Milliseconds()/1000*2 {
			update = append(update, key)
			log.Print("refreshRecordsByCacheTimeout update")
		}
		log.Printf("refreshRecordsByCacheTimeout, key: %v, entry: %v, timeDiff:%v, refreshTime:%v", key, entry, entry.expire-time.Now().Unix(), r.RefreshTime.Milliseconds()/1000*2)

	}
	r.mu.RUnlock()

	// 如果使用了 cacheExpireUnused 策略，则不使用了 clearUnused 策略
	isUsed := false
	if cacheExpireUnused {
		isUsed = true
	}
	for _, key := range update {
		// todo ,待测试打印，循环和日志打印地方
		rrs, err := r.update(context.Background(), key, isUsed, persistOnFailure)
		if err != nil {
			log.Printf("update dnscache has some error, key: %v, rrs: %v, err:%v", key, rrs, err)
		}
	}
}

func (r *Resolver) Refresh(clearUnused bool) {
	r.refreshRecords(clearUnused, false, false)
}

func (r *Resolver) RefreshWithOptions(options ResolverRefreshOptions) {
	r.refreshRecords(options.ClearUnused, options.PersistOnFailure, options.CacheExpireUnused)
}

func (r *Resolver) init() {
	r.cache = make(map[string]*cacheEntry)
}

// lookupGroup merges lookup calls together for lookups for the same host. The
// lookupGroup key is is the LookupIPAddr.host argument.
var lookupGroup singleflight.Group

func (r *Resolver) lookup(ctx context.Context, key string) (rrs []string, err error) {
	var found bool
	rrs, err, found = r.load(key)
	if !found {
		if r.OnCacheMiss != nil {
			r.OnCacheMiss()
		}
		rrs, err = r.update(ctx, key, true, false)
	}
	return
}

func (r *Resolver) update(ctx context.Context, key string, used bool, persistOnFailure bool) (rrs []string, err error) {
	c := lookupGroup.DoChan(key, r.lookupFunc(ctx, key))
	select {
	case <-ctx.Done():
		err = ctx.Err()
		if err == context.DeadlineExceeded {
			// If DNS request timed out for some reason, force future
			// request to start the DNS lookup again rather than waiting
			// for the current lookup to complete.
			lookupGroup.Forget(key)
		}
	case res := <-c:
		if res.Shared {
			// We had concurrent lookups, check if the cache is already updated
			// by a friend.
			var found bool
			rrs, err, found = r.load(key)
			if found {
				return
			}
		}
		err = res.Err
		if err == nil {
			rrs, _ = res.Val.([]string)
		}

		if err != nil && persistOnFailure {
			var found bool
			rrs, err, found = r.load(key)
			if found {
				return
			}
		}

		r.mu.Lock()
		r.storeLocked(key, rrs, used, err)
		r.mu.Unlock()
	}
	return
}

// lookupFunc returns lookup function for key. The type of the key is stored as
// the first char and the lookup subject is the rest of the key.
func (r *Resolver) lookupFunc(ctx context.Context, key string) func() (interface{}, error) {
	if len(key) == 0 {
		panic("lookupFunc with empty key")
	}

	var resolver DNSResolver = defaultResolver
	if r.Resolver != nil {
		resolver = r.Resolver
	}

	switch key[0] {
	case 'h':
		return func() (interface{}, error) {
			ctx, cancel := r.prepareCtx(ctx)
			defer cancel()

			return resolver.LookupHost(ctx, key[1:])
		}
	case 'r':
		return func() (interface{}, error) {
			ctx, cancel := r.prepareCtx(ctx)
			defer cancel()

			return resolver.LookupAddr(ctx, key[1:])
		}
	default:
		panic("lookupFunc invalid key type: " + key)
	}
}

func (r *Resolver) prepareCtx(origContext context.Context) (ctx context.Context, cancel context.CancelFunc) {
	ctx = context.Background()
	if r.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
	} else {
		cancel = func() {}
	}

	// If a httptrace has been attached to the given context it will be copied over to the newly created context. We only need to copy pointers
	// to DNSStart and DNSDone hooks
	if trace := httptrace.ContextClientTrace(origContext); trace != nil {
		derivedTrace := &httptrace.ClientTrace{
			DNSStart: trace.DNSStart,
			DNSDone:  trace.DNSDone,
		}

		ctx = httptrace.WithClientTrace(ctx, derivedTrace)
	}

	return
}

func (r *Resolver) load(key string) (rrs []string, err error, found bool) {
	r.mu.RLock()
	var entry *cacheEntry
	entry, found = r.cache[key]
	if !found {
		r.mu.RUnlock()
		return
	}
	rrs = entry.rrs
	err = entry.err
	used := entry.used
	r.mu.RUnlock()
	if !used {
		r.mu.Lock()
		entry.used = true
		r.mu.Unlock()
	}
	return rrs, err, true
}

func (r *Resolver) storeLocked(key string, rrs []string, used bool, err error) {
	if entry, found := r.cache[key]; found {
		// Update existing entry in place
		entry.rrs = rrs
		entry.err = err
		entry.used = used
		entry.expire = time.Now().Unix() + r.getCacheTimeOut().Milliseconds()/1000
		return
	}
	r.cache[key] = &cacheEntry{
		rrs:    rrs,
		err:    err,
		used:   used,
		expire: time.Now().Unix() + r.getCacheTimeOut().Milliseconds()/1000,
	}
}

func (r *Resolver) getCacheTimeOut() time.Duration {
	if r.CacheTimeout == 0 {
		return DefaultCacheTimeout
	}
	return r.CacheTimeout
}

var defaultResolver = &defaultResolverWithTrace{}

// defaultResolverWithTrace calls `LookupIP` instead of `LookupHost` on `net.DefaultResolver` in order to cause invocation of the `DNSStart`
// and `DNSDone` hooks. By implementing `DNSResolver`, backward compatibility can be ensured.
type defaultResolverWithTrace struct{}

func (d *defaultResolverWithTrace) LookupHost(ctx context.Context, host string) (addrs []string, err error) {
	// `net.Resolver#LookupHost` does not cause invocation of `net.Resolver#lookupIPAddr`, therefore the `DNSStart` and `DNSDone` tracing hooks
	// built into the stdlib are never called. `LookupIP`, despite it's name, can also be used to lookup a hostname but does cause these hooks to be
	// triggered. The format of the response is different, therefore it needs this thin wrapper converting it.
	rawIPs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}

	cookedIPs := make([]string, len(rawIPs))

	for i, v := range rawIPs {
		cookedIPs[i] = v.String()
	}

	return cookedIPs, nil
}

func (d *defaultResolverWithTrace) LookupAddr(ctx context.Context, addr string) (names []string, err error) {
	return net.DefaultResolver.LookupAddr(ctx, addr)
}
