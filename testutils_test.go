package dnscache

import (
	"context"
	"errors"
)

type BadResolver struct {
	choke bool
}

func (r BadResolver) LookupAddr(ctx context.Context, addr string) (names []string, err error) {
	return
}

func (r BadResolver) LookupHost(ctx context.Context, host string) (addrs []string, err error) {
	if r.choke {
		err = errors.New("Look Up Failed")
	} else {
		addrs = []string{"216.58.192.238"}
	}
	return
}
