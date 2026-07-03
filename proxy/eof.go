// Package proxy — eof.go
//
// ioEOF is a tiny bridge that returns io.EOF from the "io" package.
// It allows tls.go to reference io.EOF via an internal function rather
// than importing "io" directly at the top level (which would duplicate
// the import already present in tcp.go).
package proxy

import "io"

func ioEOF() error { return io.EOF }
