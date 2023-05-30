package handler

import (
	"github.com/matrix-org/sliding-sync/sync2"
	"sync"

	"github.com/matrix-org/sliding-sync/pubsub"
)

// pendingInfo tracks the status of a poller that we are (or previously were) waiting
// to start.
type pendingInfo struct {
	// done is set to true when we confirm that this poller has started polling.
	done bool
	// ch is a dummy channel which never receives any data. A call to
	// EnsurePoller.OnInitialSyncComplete will close the channel (unblocking any
	// EnsurePoller.EnsurePolling calls which are waiting on it) and then set the ch
	// field to nil.
	ch chan struct{}
}

// EnsurePoller is a gadget used by the sliding sync request handler to ensure that
// we are running a v2 poller for a given device.
type EnsurePoller struct {
	chanName string
	// mu guards reads and writes to pendingPolls.
	mu *sync.Mutex
	// pendingPolls tracks the status of pollers that we are waiting to start.
	pendingPolls map[sync2.PollerID]pendingInfo
	notifier     pubsub.Notifier
}

func NewEnsurePoller(notifier pubsub.Notifier) *EnsurePoller {
	return &EnsurePoller{
		chanName:     pubsub.ChanV3,
		mu:           &sync.Mutex{},
		pendingPolls: make(map[sync2.PollerID]pendingInfo),
		notifier:     notifier,
	}
}

// EnsurePolling blocks until the V2InitialSyncComplete response is received for this device. It is
// the caller's responsibility to call OnInitialSyncComplete when new events arrive.
func (p *EnsurePoller) EnsurePolling(pid sync2.PollerID, tokenHash string) {
	p.mu.Lock()
	// do we need to wait?
	if p.pendingPolls[pid].done {
		p.mu.Unlock()
		return
	}
	// have we called EnsurePolling for this user/device before?
	ch := p.pendingPolls[pid].ch
	if ch != nil {
		p.mu.Unlock()
		// we already called EnsurePolling on this device, so just listen for the close
		// TODO: several times there have been problems getting the response back from the poller
		// we should time out here after 100s and return an error or something to kick conns into
		// trying again
		<-ch
		return
	}
	// Make a channel to wait until we have done an initial sync
	ch = make(chan struct{})
	p.pendingPolls[pid] = pendingInfo{
		done: false,
		ch:   ch,
	}
	p.mu.Unlock()
	// ask the pollers to poll for this device
	p.notifier.Notify(p.chanName, &pubsub.V3EnsurePolling{
		UserID:          pid.UserID,
		DeviceID:        pid.DeviceID,
		AccessTokenHash: tokenHash,
	})
	// if by some miracle the notify AND sync completes before we receive on ch then this is
	// still fine as recv on a closed channel will return immediately.
	<-ch
}

func (p *EnsurePoller) OnInitialSyncComplete(payload *pubsub.V2InitialSyncComplete) {
	pid := sync2.PollerID{UserID: payload.UserID, DeviceID: payload.DeviceID}
	p.mu.Lock()
	defer p.mu.Unlock()
	pending, ok := p.pendingPolls[pid]
	// were we waiting for this initial sync to complete?
	if !ok {
		// This can happen when the v2 poller spontaneously starts polling even without us asking it to
		// e.g from the database
		p.pendingPolls[pid] = pendingInfo{
			done: true,
		}
		return
	}
	if pending.done {
		// nothing to do, we just got OnInitialSyncComplete called twice
		return
	}
	// we get here if we asked the poller to start via EnsurePolling, so let's make that goroutine
	// wake up now
	ch := pending.ch
	pending.done = true
	pending.ch = nil
	p.pendingPolls[pid] = pending
	close(ch)
}

func (p *EnsurePoller) OnExpiredToken(payload *pubsub.V2ExpiredToken) {
	pid := sync2.PollerID{UserID: payload.UserID, DeviceID: payload.DeviceID}
	p.mu.Lock()
	defer p.mu.Unlock()
	pending, exists := p.pendingPolls[pid]
	if !exists {
		// We weren't tracking the state of this poller, so we have nothing to clean up.
		return
	}
	if pending.ch != nil {
		// unblock anything waiting on this sync, it's never going to happen now.
		close(pending.ch)
	}
	delete(p.pendingPolls, pid)
}

func (p *EnsurePoller) Teardown() {
	p.notifier.Close()
}
