//go:build !http3

package main

import (
	"crypto/tls"
	"errors"
	"net/http"
)

// h3Server is the optional HTTP/3 listener.
type h3Server interface {
	ListenAndServe() error
	Close() error
}

func newHTTP3(string, http.Handler, *tls.Config) (h3Server, error) {
	return nil, errors.New("tls.http3 is enabled in the config, but this binary was built without HTTP/3 support; rebuild with: go build -tags http3")
}
