// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package autocert

import (
	"context"
	"crypto"
	"sync"
	"time"
)

// renewJitter is the maximum deviation from Manager.RenewBefore.
const renewJitter = time.Hour

// domainRenewal tracks the state used by the periodic timers
// renewing a single domain's cert.
type domainRenewal struct {
	m      *Manager
	domain string
	key    crypto.Signer

	timerMu sync.Mutex
	timer   *time.Timer
}

// start starts a cert renewal timer at the time
// defined by the certificate expiration time exp.
//
// If the timer is already started, calling start is a noop.
func (dr *domainRenewal) start(exp time.Time) {
	dr.timerMu.Lock()
	defer dr.timerMu.Unlock()
	if dr.timer != nil {
		return
	}
	dr.timer = time.AfterFunc(dr.next(exp), dr.renew)
}

// stop stops the cert renewal timer.
// If the timer is already stopped, calling stop is a noop.
func (dr *domainRenewal) stop() {
	dr.timerMu.Lock()
	defer dr.timerMu.Unlock()
	if dr.timer == nil {
		return
	}
	dr.timer.Stop()
	dr.timer = nil
}

// renew is called periodically by a timer.
// The first renew call is kicked off by dr.start.
func (dr *domainRenewal) renew() {
	dr.timerMu.Lock()
	defer dr.timerMu.Unlock()
	if dr.timer == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	// TODO: rotate dr.key at some point?
	next, err := dr.do(ctx)
	if err != nil {
		next = renewJitter / 2
		next += time.Duration(pseudoRand.int63n(int64(next)))
	}
	dr.timer = time.AfterFunc(next, dr.renew)
	testDidRenewLoop(next, err)
}

// do is similar to Manager.createCert but it doesn't lock a Manager.state item.
// Instead, it requests a new certificate independently and, upon success,
// replaces dr.m.state item with a new one and updates cache for the given domain.
//
// It may return immediately if the expiration date of the currently cached cert
// is far enough in the future.
//
// The returned value is a time interval after which the renewal should occur again.
func (dr *domainRenewal) do(ctx context.Context) (time.Duration, error) {
	// a race is likely unavoidable in a distributed environment
	// but we try nonetheless
	if tlscert, err := dr.m.cacheGet(ctx, dr.domain); err == nil {
		next := dr.next(tlscert.Leaf.NotAfter)
		if next > dr.m.renewBefore()+renewJitter {
			return next, nil
		}
	}

	der, leaf, err := dr.m.authorizedCert(ctx, dr.key, dr.domain)
	if err != nil {
		return 0, err
	}
	state := &certState{
		key:  dr.key,
		cert: der,
		leaf: leaf,
	}
	tlscert, err := state.tlscert()
	if err != nil {
		return 0, err
	}
	dr.m.cachePut(ctx, dr.domain, tlscert)
	dr.m.stateMu.Lock()
	defer dr.m.stateMu.Unlock()
	// m.state is guaranteed to be non-nil at this point
	dr.m.state[dr.domain] = state
	return dr.next(leaf.NotAfter), nil
}

func (dr *domainRenewal) next(expiry time.Time) time.Duration {
	d := expiry.Sub(timeNow()) - dr.m.renewBefore()
	// add a bit of randomness to renew deadline
	n := pseudoRand.int63n(int64(renewJitter))
	d -= time.Duration(n)
	if d < 0 {
		return 0
	}
	return d
}

var testDidRenewLoop = func(next time.Duration, err error) {}

type ocspUpdater struct {
	m      *Manager
	domain string

	timerMu sync.Mutex
	timer   *time.Timer
}

func (ou *ocspUpdater) start(next time.Time) {
	ou.timerMu.Lock()
	defer ou.timerMu.Unlock()
	if ou.timer != nil {
		return
	}
	ou.timer = time.AfterFunc(ou.next(next), ou.update)
}

func (ou *ocspUpdater) stop() {
	ou.timerMu.Lock()
	defer ou.timerMu.Unlock()
	if ou.timer == nil {
		return
	}
	ou.timer.Stop()
	ou.timer = nil
}

func (ou *ocspUpdater) update() {
	ou.timerMu.Lock()
	defer ou.timerMu.Unlock()
	if ou.timer == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var next time.Duration
	// state will not be nil
	state, _ := ou.m.ocspStates[ou.domain]
	der, response, err := ou.m.updateOCSPStapling(ctx, state.leaf, state.issuer)
	if err != nil {
		// failed
		next = renewJitter / 2
		next += time.Duration(pseudoRand.int63n(int64(next)))
	} else {
		// success
		state.Lock()
		defer state.Unlock()
		state.ocspDER = der
		state.nextUpdate = response.NextUpdate
		next = ou.next(response.NextUpdate)
	}
	ou.timer = time.AfterFunc(next, ou.update)
	testOCSPDidUpdateLoop(next, err)
}

func (ou *ocspUpdater) next(expiry time.Time) time.Duration {
	d := expiry.Sub(timeNow()) - 48*time.Hour
	// add a bit randomness to renew deadline
	n := pseudoRand.int63n(int64(renewJitter))
	d -= time.Duration(n)
	if d < 0 {
		return 0
	}
	return d
}

var testOCSPDidUpdateLoop = func(next time.Duration, err error) {}