package dnscache

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Resolver adds a DNS cache layer to Go's net.Resolver.
type Resolver struct {
	// Timeout defines the maximum allowed time allowed for a lookup.
	Timeout time.Duration

	// Resolver is the net.Resolver used to perform actual DNS lookup. If nil,
	// net.DefaultResolver is used instead.
	Resolver *net.Resolver

	once  sync.Once
	mu    sync.RWMutex
	cache map[string]*cacheEntry

	onCacheMiss func()
}

type cacheEntry struct {
	rrs  []string
	err  error
	used bool
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

// Refresh refreshes cached entries which has been used at least once since the
// last Refresh. If clearUnused is true, entries which hasn't be used since the
// last Refresh are removed from the cache.
func (r *Resolver) Refresh(clearUnused bool) {
	r.once.Do(r.init)
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
		r.update(context.Background(), key, false)
	}
}

// Dial is a function to use in DialContext for http.Transport.
// It looks up the host via Resolver and passes it to net.Dial.
//
// Be warned that this is a simplistic implementation and misses a lot of features like dual stack support,
// correct handling of context tracing, randomization of adds etc.
// This implementation will also choke on "unix", "unixgram" and "unixpacket" network.
func (r *Resolver) Dial(ctx context.Context, network string, addr string) (conn net.Conn, err error) {
	separator := strings.LastIndex(addr, ":")
	ips, err := r.LookupHost(ctx, addr[:separator])
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		conn, err = net.Dial(network, ip+addr[separator:])
		if err == nil {
			break
		}
	}
	return
}

func (r *Resolver) init() {
	r.cache = make(map[string]*cacheEntry)
}

// lookupGroup merges lookup calls together for lookups for the same host. The
// lookupGroup key is is the LookupIPAddr.host argument.
var lookupGroup singleflight.Group

func (r *Resolver) lookup(ctx context.Context, key string) (rrs []string, err error) {
	var found bool
	rrs, found, err = r.load(key)
	if !found {
		if r.onCacheMiss != nil {
			r.onCacheMiss()
		}
		rrs, err = r.update(ctx, key, true)
	}
	return
}

func (r *Resolver) update(ctx context.Context, key string, used bool) (rrs []string, err error) {
	c := lookupGroup.DoChan(key, r.lookupFunc(key))
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
			rrs, found, err = r.load(key)
			if found {
				return
			}
		}
		err = res.Err
		if err == nil {
			rrs, _ = res.Val.([]string)
		}
		r.mu.Lock()
		r.storeLocked(key, rrs, used, err)
		r.mu.Unlock()
	}
	return
}

// lookupFunc returns lookup function for key. The type of the key is stored as
// the first char and the lookup subject is the rest of the key.
func (r *Resolver) lookupFunc(key string) func() (interface{}, error) {
	if len(key) == 0 {
		panic("lookupFunc with empty key")
	}
	resolver := net.DefaultResolver
	if r.Resolver != nil {
		resolver = r.Resolver
	}
	switch key[0] {
	case 'h':
		return func() (interface{}, error) {
			ctx, cancel := r.getCtx()
			defer cancel()
			return resolver.LookupHost(ctx, key[1:])
		}
	case 'r':
		return func() (interface{}, error) {
			ctx, cancel := r.getCtx()
			defer cancel()
			return resolver.LookupAddr(ctx, key[1:])
		}
	default:
		panic("lookupFunc invalid key type: " + key)
	}
}

func (r *Resolver) getCtx() (ctx context.Context, cancel context.CancelFunc) {
	ctx = context.Background()
	if r.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
	} else {
		cancel = func() {}
	}
	return
}

func (r *Resolver) load(key string) (rrs []string, found bool, err error) {
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
	return rrs, true, err
}

func (r *Resolver) storeLocked(key string, rrs []string, used bool, err error) {
	if entry, found := r.cache[key]; found {
		// Update existing entry in place
		entry.rrs = rrs
		entry.err = err
		entry.used = used
		return
	}
	r.cache[key] = &cacheEntry{
		rrs:  rrs,
		err:  err,
		used: used,
	}
}
