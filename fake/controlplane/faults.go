package controlplane

import (
	"net/http"
	"strings"
)

// The injected faults. Every one of them is a row in section 18 of the runtime
// contract: the checklist asks what the image does when a PUT comes back 403,
// when the controller is down for the whole retry budget, when the prompt link
// leads nowhere. None of those can be provoked by writing a better test — they
// need a control plane willing to misbehave on request.
//
// Faults are states, not scripts. A test says "storage is refusing writes now",
// looks at what the image did, and turns it off; it does not describe a
// sequence the fake then replays, because a sequence has to be kept in step
// with an implementation that is still being written.

type faults struct {
	// storage maps a method to the answer it gives, optionally narrowed to keys
	// containing a substring. Narrowing matters: "the push failed but the
	// result was already durable" needs persist to succeed and finalize to
	// fail, and a blanket fault cannot express that.
	storage []storageFaultRule

	// callbackStatus is the answer the completion endpoint gives while
	// callbackRemaining is non-zero. Remaining counts down, and -1 means
	// forever — which is how "the controller is unreachable for all 60 seconds
	// of retries" is set up.
	callbackStatus    int
	callbackRemaining int
	callbackAttempts  int
}

type storageFaultRule struct {
	method    string
	keyMatch  string
	status    int
	code      string
	remaining int
}

// FailStorage makes storage answer status to matching requests.
//
// method is GET, PUT or POST, or empty for all three. keyMatch narrows the rule
// to keys containing that substring, or empty for every key. count is how many
// matching requests to fail; a non-positive count means until the rule is
// cleared, which is stored as -1 so that a rule counting down to zero and a
// rule that never runs out stay distinguishable.
func (c *ControlPlane) FailStorage(method, keyMatch string, status, count int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if count <= 0 {
		count = forever
	}
	c.faults.storage = append(c.faults.storage, storageFaultRule{
		method:    strings.ToUpper(method),
		keyMatch:  keyMatch,
		status:    status,
		code:      storageCodeFor(status),
		remaining: count,
	})
}

// forever marks a fault with no expiry. Zero cannot serve: a rule that has
// counted down to zero is exhausted, and one asked to last forever is not.
const forever = -1

// ClearStorageFaults removes every storage rule.
func (c *ControlPlane) ClearStorageFaults() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.faults.storage = nil
}

// storageFault decides whether this request is one the fake is refusing. The
// caller holds the lock.
func (c *ControlPlane) storageFault(method, key string) (int, string) {
	for i := range c.faults.storage {
		rule := &c.faults.storage[i]
		if rule.remaining == 0 {
			continue
		}
		if rule.method != "" && rule.method != method {
			continue
		}
		if rule.keyMatch != "" && !strings.Contains(key, rule.keyMatch) {
			continue
		}
		if rule.remaining > 0 {
			rule.remaining--
		}
		return rule.status, rule.code
	}
	return 0, ""
}

// storageCodeFor gives the S3 error code that goes with a status, so that an
// injected failure is indistinguishable from the real one in the image's logs.
func storageCodeFor(status int) string {
	switch status {
	case http.StatusForbidden:
		return "AccessDenied"
	case http.StatusNotFound:
		return "NoSuchKey"
	case http.StatusRequestEntityTooLarge:
		return "EntityTooLarge"
	case http.StatusServiceUnavailable:
		return "SlowDown"
	default:
		return "InternalError"
	}
}

// FailCallback makes the completion endpoint answer status for the next count
// deliveries. A non-positive count means every delivery until it is cleared —
// the setup for the checklist row that says an undelivered webhook must not
// change the exit code.
func (c *ControlPlane) FailCallback(status, count int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if count <= 0 {
		count = forever
	}
	c.faults.callbackStatus = status
	c.faults.callbackRemaining = count
}

// ClearCallbackFaults restores the endpoint.
func (c *ControlPlane) ClearCallbackFaults() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.faults.callbackStatus = 0
	c.faults.callbackRemaining = 0
}

// callbackFault decides whether this delivery is refused. The caller holds the
// lock, and has already counted the attempt.
func (c *ControlPlane) callbackFault() int {
	if c.faults.callbackStatus == 0 || c.faults.callbackRemaining == 0 {
		return 0
	}
	if c.faults.callbackRemaining > 0 {
		c.faults.callbackRemaining--
	}
	return c.faults.callbackStatus
}
