package dnscache

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
)

func Example() {
	r := New(net.DefaultResolver)
	t := &http.Transport{
		DialContext: func(ctx context.Context, network string, addr string) (conn net.Conn, err error) {
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
		},
	}
	c := &http.Client{Transport: t}
	res, err := c.Get("http://httpbin.org/status/418")
	if err == nil {
		fmt.Println(res.StatusCode)
	}
	// Output: 418
}
