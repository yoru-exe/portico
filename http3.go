//go:build http3

package main

import (
	"crypto/tls"
	"net/http"

	"github.com/quic-go/quic-go/http3"
)

// h3Server is the optional HTTP/3 listener.
type h3Server interface {
	ListenAndServe() error
	Close() error
}

func newHTTP3(addr string, h http.Handler, tc *tls.Config) (h3Server, error) {
	return &http3.Server{Addr: addr, Handler: h, TLSConfig: tc}, nil
}
