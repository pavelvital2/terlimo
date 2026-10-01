package main

// Service-channel wiring: the single injection point for the mobile service Doer and
// the client-side establishment. It is additive and does not touch accountaccess,
// the WLBS codec, admission, the probe map or the group/pacer.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"

	"wg-turn-client/accountaccess"
	"wg-turn-client/servicechannel"
)

// Compile-time proof that the seam satisfies the existing accountaccess.Doer.
var _ accountaccess.Doer = (*servicechannel.Doer)(nil)

// newMobileTransport selects the transport for the whole mobile-v1 client:
// without a service seed (and without durable service state) the accepted bounded
// HTTPS client is returned unchanged; with a trusted seed every mobile API call goes
// through the service channel only. A durable service state without the packaged
// builtin seed is a configuration error, never a silent direct-HTTPS fallback.
func newMobileTransport(start managedStart, persist servicechannel.PersistFunc) (accountaccess.Doer, error) {
	transport, _, err := newMobileTransportAndStore(start, persist)
	return transport, err
}

// newMobileTransportAndStore is the same selection, additionally returning the
// configured service seed store so the explicit onboarding bootstrap dialer can reuse
// the existing public VK hash list and stream id. Without a seed the store is nil and
// the onboarding starter fails closed instead of inventing credentials.
func newMobileTransportAndStore(start managedStart, persist servicechannel.PersistFunc) (accountaccess.Doer, *servicechannel.Store, error) {
	if start.ServiceSeed == "" {
		if start.ServiceSeedStateB64 != "" {
			return nil, nil, servicechannel.ErrSeedMissing
		}
		return &http.Client{Timeout: mobileHTTPTimeout}, nil, nil
	}
	environment := string(accountaccess.PoPEnvironment(start.MobileEnvironment))
	if environment == "" {
		environment = string(accountaccess.EnvironmentTest)
	}
	seeds := servicechannel.NewStore(servicechannel.Config{Environment: environment, Persist: persist})
	if err := seeds.Update(servicechannel.SourceBuiltin, []byte(start.ServiceSeed)); err != nil {
		return nil, nil, err
	}
	if start.ServiceSeedStateB64 != "" {
		blob, err := base64.RawURLEncoding.DecodeString(start.ServiceSeedStateB64)
		if err != nil {
			return nil, nil, servicechannel.ErrSeedInvalid
		}
		if err := seeds.LoadState(blob); err != nil {
			return nil, nil, err
		}
	}
	channel := servicechannel.NewChannel(servicechannel.EstablishFunc(managedServiceEstablish))
	doer, err := servicechannel.NewDoer(start.MobileBaseURL, seeds, channel)
	if err != nil {
		return nil, nil, err
	}
	// Fixed stage names only; stderr is filtered again by the host allowlist.
	observe := func(stage string) { fmt.Fprintln(os.Stderr, "svcstage:", stage) }
	channel.Observe = observe
	doer.Observe = observe
	// Secret-free per-session correlation: session generation, local monotonic exchange
	// id, fixed request class, fixed event, reuse flag, bounded counters/durations and a
	// fixed error class (never IP/credential/token/payload/raw error text).
	channel.Trace = func(ev servicechannel.TraceEvent) {
		fmt.Fprintln(os.Stderr, formatServiceTrace(ev))
	}
	return doer, seeds, nil
}

// formatServiceTrace renders one fixed svctrace line. All values are bounded integers,
// fixed uppercase classes/tokens or local counters; no payload-bearing field exists.
func formatServiceTrace(ev servicechannel.TraceEvent) string {
	flag := 0
	if ev.Reused {
		flag = 1
	}
	line := fmt.Sprintf("svctrace: gen=%d class=%s event=%s reused=%d port=%d xid=%d elapsed_ms=%d",
		ev.Session, ev.Class, ev.Event, flag, ev.Port, ev.Exchange, ev.ElapsedMS)
	if ev.Requests > 0 {
		line += fmt.Sprintf(" req=%d", ev.Requests)
	}
	if ev.IdleMS > 0 {
		line += fmt.Sprintf(" idle_ms=%d", ev.IdleMS)
	}
	if ev.ErrClass != "" && ev.ErrClass != "NONE" {
		line += " err=" + ev.ErrClass
	}
	return line
}

// managedServiceEstablish is the production establishment: it derives the WRAP key
// from the public service classifier with the same deriveWrapKey mechanism the node
// transport uses (SetServiceClassifier on the node side), fetches the existing VK
// credentials through GetCreds including the hash fallback, and dials the pinned DTLS
// transport through dialManagedTransport (child-only physical bind). It returns a
// service DTLS net.Conn plus cleanup and never touches catalog, grant, probe-map,
// GETCONF/MUX or the worker pool. A wrong pin fails TRUST_FAILED and a wrong key
// never completes the handshake.
func managedServiceEstablish(ctx context.Context, seed servicechannel.Seed) (net.Conn, func(), error) {
	if err := seed.Validate(); err != nil {
		return nil, nil, err
	}
	if _, err := managedPinVerifier(seed.DTLSSPKISHA256); err != nil {
		return nil, nil, err
	}
	key, err := deriveWrapKey(seed.ServiceClassifier)
	if err != nil {
		return nil, nil, servicechannel.ErrSeedInvalid
	}
	fmt.Fprintln(os.Stderr, "svcstage: ESTABLISH_SEED_READY")
	var user, pass string
	var urls []string
	fmt.Fprintln(os.Stderr, "svcstage: ESTABLISH_VK_BEGIN")
	// HASH_BEGIN/END are optional hash-fallback noise: emit them only when there is more
	// than one candidate. Only the fixed candidate index travels here, never the hash.
	multiHash := len(seed.VKHashes) > 1
	for index, hash := range seed.VKHashes {
		if multiHash {
			diagEmitVK(vkStageHashBegin, index)
		}
		user, pass, urls, err = GetCreds(ctx, hash, seed.StreamID)
		if multiHash {
			diagEmitVK(vkStageHashEnd, index)
		}
		if err == nil || index == len(seed.VKHashes)-1 || !isHashFallbackCredentialError(err) {
			break
		}
	}
	if err != nil {
		return nil, nil, errors.New("VK_API_UNAVAILABLE")
	}
	fmt.Fprintln(os.Stderr, "svcstage: ESTABLISH_VK_READY")
	peer := &net.UDPAddr{IP: net.ParseIP(seed.PeerIP), Port: seed.DTLSPort}
	if peer.IP == nil {
		return nil, nil, servicechannel.ErrSeedInvalid
	}
	tp := &TurnParams{WrapKey: key, Hashes: seed.VKHashes}
	creds := &Credentials{User: user, Pass: pass, TurnURLs: urls, CacheStreamID: seed.StreamID}
	fmt.Fprintln(os.Stderr, "svcstage: ESTABLISH_DIAL_BEGIN")
	trace := newServiceDialTrace()
	dialCtx := context.WithValue(ctx, serviceDialKey{}, trace)
	conn, cleanup, err := dialManagedTransport(dialCtx, tp, peer, creds, seed.DTLSSPKISHA256, 0, false, 0, false, nil)
	trace.emit(err)
	if err != nil {
		return nil, nil, err
	}
	if conn == nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, nil, servicechannel.ErrTransportFailed
	}
	fmt.Fprintln(os.Stderr, "svcstage: ESTABLISH_DIAL_READY")
	return conn, cleanup, nil
}
