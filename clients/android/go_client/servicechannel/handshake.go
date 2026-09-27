package servicechannel

import (
	"context"
	"net"
)

// Establishment is the isolated seam between the validated public seed and the real
// service connection. The production implementation (package main) derives the WRAP
// key from the public service classifier with the exact mechanism the node transport
// uses (deriveWrapKey), fetches the existing VK credentials with the hash fallback,
// and dials the existing pinned DTLS transport (child-only physical bind). There is
// no server-issued key, no PSK and no client mTLS. Nothing above this interface
// depends on those details.
type Establishment interface {
	Establish(ctx context.Context, seed Seed) (net.Conn, func(), error)
}

// EstablishFunc adapts a plain function to the Establishment seam.
type EstablishFunc func(ctx context.Context, seed Seed) (net.Conn, func(), error)

// Establish implements Establishment.
func (f EstablishFunc) Establish(ctx context.Context, seed Seed) (net.Conn, func(), error) {
	return f(ctx, seed)
}
