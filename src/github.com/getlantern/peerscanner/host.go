package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/getlantern/cloudflare"
	"github.com/getlantern/enproxy"
	"github.com/getlantern/peerscanner/cfr"
	"github.com/getlantern/tlsdialer"
	"github.com/getlantern/withtimeout"
)

var (
	// Set a short ttl on DNS entries
	ttl = 30 * time.Second

	// Test with a period of half the ttl
	testPeriod = ttl / 2

	// If we haven't had a successul test or reset after this amount of time,
	// pause testing until receipt of next register call.
	pauseAfter = 10 * time.Minute

	// Limit how long we're willing to wait for status
	statusTimeout = ttl * 2

	dialTimeout    = 3 * time.Second // how long to wait on connecting to host
	requestTimeout = 6 * time.Second // how long to wait for test requests to process
	proxyAttempts  = 1               // how many times to try a test request before considering host down

	// Sites to use for testing connectivity. WARNING - these should only be
	// sites with consistent fast response times, around the world, otherwise
	// tests may time out.
	testSites = []string{"www.google.com", "www.youtube.com", "www.facebook.com"}
)

type status struct {
	online            bool
	connectionRefused bool
}

// host is an actor that represents a host entry in CloudFlare and is
// responsible for checking connectivity to the host and updating CloudFlare DNS
// accordingly. Once a host has been created, it sticks around ad infinitum.
// If the host hasn't heard from the real-world host in over 10 minutes, it
// pauses its processing and only resumes once it hears from the client again.
type host struct {
	name        string
	ip          string
	cdnRecord   *cloudflare.Record
	noCdnRecord *cloudflare.Record
	cfrDist     *cfr.Distribution
	cdnGroups   map[string]*group
	noCdnGroups map[string]*group
	lastSuccess time.Time
	lastTest    time.Time

	resetCh      chan string
	unregisterCh chan interface{}
	statusCh     chan chan *status
	initCfrCh    chan interface{}

	proxiedClient     *http.Client
	reportedHost      string
	reportedHostMutex sync.Mutex
}

func (h *host) String() string {
	return fmt.Sprintf("%v (%v)", h.name, h.ip)
}

/*******************************************************************************
 * API for interacting with host
 ******************************************************************************/

// newHost creates a new host for the given name, ip and optional DNS record.
func newHost(name string, ip string, record *cloudflare.Record) *host {
	// Cache TLS sessions
	clientSessionCache := tls.NewLRUClientSessionCache(1000)

	h := &host{
		name:         name,
		ip:           ip,
		cdnRecord:    record,
		resetCh:      make(chan string, 1000),
		unregisterCh: make(chan interface{}, 1),
		statusCh:     make(chan chan *status, 1000),
		initCfrCh:    make(chan interface{}, 1),
	}
	h.proxiedClient = &http.Client{
		Transport: &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				return enproxy.Dial(addr, &enproxy.Config{
					DialProxy: func(addr string) (net.Conn, error) {
						return tlsdialer.DialWithDialer(&net.Dialer{
							Timeout: dialTimeout,
						}, "tcp", ip+":443", true, &tls.Config{
							InsecureSkipVerify: true,
							ClientSessionCache: clientSessionCache,
						})
					},
					NewRequest: func(upstreamHost string, method string, body io.Reader) (req *http.Request, err error) {
						return http.NewRequest(method, "http://"+ip+"/", body)
					},
					OnFirstResponse: func(resp *http.Response) {
						h.reportedHostMutex.Lock()
						h.reportedHost = resp.Header.Get(enproxy.X_ENPROXY_PROXY_HOST)
						h.reportedHostMutex.Unlock()
					},
				})
			},
			DisableKeepAlives: true,
		},
		Timeout: requestTimeout,
	}

	if h.isFallback() {
		h.cdnGroups = map[string]*group{
			RoundRobin: &group{subdomain: RoundRobin},
			Fallbacks:  &group{subdomain: Fallbacks},
		}
		h.noCdnGroups = map[string]*group{
			RoundRobinNoCdn: &group{subdomain: RoundRobinNoCdn},
			FallbacksNoCdn:  &group{subdomain: FallbacksNoCdn},
		}
	} else {
		h.cdnGroups = map[string]*group{
			Peers: &group{subdomain: Peers},
		}
		h.noCdnGroups = map[string]*group{
			PeersNoCdn: &group{subdomain: PeersNoCdn},
		}
	}

	return h
}

// status returns the status of this host as of the next scheduled check
func (h *host) status() (online bool, connectionRefused bool, timedOut bool) {
	// Buffer the channel so that if we time out, reportStatus can still report
	// without blocking.
	sch := make(chan *status, 1)
	h.statusCh <- sch
	select {
	case s := <-sch:
		return s.online, s.connectionRefused, false
	case <-time.After(statusTimeout):
		return false, false, true
	}
}

// reset resets this host's run loop in response to the host having reported in,
// which can include changing the name if the given name is new.
func (h *host) reset(newName string) {
	h.resetCh <- newName
}

// unregister unregisters this host in response to the host having requested
// unregistration.
func (h *host) unregister() {
	select {
	case h.unregisterCh <- nil:
		log.Tracef("Unregistering host %v", h)
	default:
		log.Tracef("Already unregistering host %v, ignoring new request", h)
	}
}

func (h *host) initCloudfront() {
	h.initCfrCh <- nil
}

func (h *host) doInitCfrDist() {
	if h.cfrDist != nil && h.cfrDist.Status == "InProgress" {
		cfr.RefreshStatus(cfrutil, h.cfrDist)
	}
	if h.cfrDist == nil {
		subdomain := strings.Split(h.name, ".")[0]
		dist, err := cfr.CreateDistribution(
			cfrutil,
			subdomain,
			noCdnPrefix+h.name,
			"created by peerscanner",
		)
		if err == nil {
			h.cfrDist = dist
		} else {
			log.Debugf("Error trying to initialize cloudfront distribution for %v: %v", h, err)
		}
	}
}

/*******************************************************************************
 * Implementation
 ******************************************************************************/

// run is the main run loop for this host
func (h *host) run() {
	checkImmediately := true
	h.lastSuccess = time.Now()
	h.lastTest = time.Now()
	periodTimer := time.NewTimer(0)
	pauseTimer := time.NewTimer(0)

	for {
		if !checkImmediately {
			// Limit the rate at which we run tests
			waitTime := h.lastTest.Add(testPeriod).Sub(time.Now())
			log.Tracef("Waiting %v until testing %v", waitTime, h)
			periodTimer.Reset(waitTime)
		}

		// Pause run loop after some largish amount of time
		pauseTimer.Reset(h.lastSuccess.Add(pauseAfter).Sub(time.Now()))

		select {
		case newName := <-h.resetCh:
			h.doReset(newName)
		case <-h.unregisterCh:
			log.Debugf("Unregistering %v and pausing", h)
			h.pause()
			checkImmediately = true
		case <-h.initCfrCh:
			h.doInitCfrDist()
		case <-pauseTimer.C:
			log.Debugf("%v had no successful checks or resets in %v, pausing", h, pauseAfter)
			h.pause()
			checkImmediately = true
		case <-periodTimer.C:
			log.Tracef("Testing %v", h)
			_s, timedOut, err := withtimeout.Do(ttl, func() (interface{}, error) {
				online, connectionRefused, err := h.isAbleToProxy()
				return &status{online, connectionRefused}, err
			})
			s := &status{false, false}
			if timedOut {
				log.Debugf("Testing %v timed out unexpectedly", h)
			}
			if _s != nil {
				s = _s.(*status)
			}
			h.reportStatus(s)
			h.lastTest = time.Now()
			checkImmediately = false
			if s.online {
				log.Tracef("Test for %v successful", h)
				h.lastSuccess = time.Now()
				err := h.register()
				if err != nil {
					log.Error(err)
				}
			} else {
				log.Tracef("Test for %v failed with error: %v", h, err)
				// Deregister this host from its rotations. We leave the host
				// itself registered to support continued sticky routing in case
				// any clients still have connections open to it.
				h.deregisterFromRotations()
			}
		}
	}
}

// pause deregisters this host completely and then waits for the next reset
// before continuing
func (h *host) pause() {
	h.deregister()
	log.Debugf("%v paused", h)
	for {
		select {
		case newName := <-h.resetCh:
			log.Debugf("Unpausing checks for %v", h)
			h.doReset(newName)
			return
		case <-h.unregisterCh:
			log.Tracef("Ignoring unregister while paused")
		}
	}
}

// reportStatus reports the given status back to any callers that are waiting
// for it.
func (h *host) reportStatus(s *status) {
	for {
		select {
		case sch := <-h.statusCh:
			sch <- s
		default:
			return
		}
	}
}

func (h *host) doReset(newName string) {
	log.Tracef("Host notified us of its presence")
	h.lastSuccess = time.Now()
	h.lastTest = time.Time{}
	if newName != h.name {
		log.Debugf("Hostname for %v changed to %v", h, newName)
		if h.cdnRecord != nil {
			log.Debugf("Deregistering old hostname %v", h.name)
			h.doDeregisterCdnHost()
			h.doDeregisterNoCdnHost()
		}
		h.name = newName
	}
}

/*******************************************************************************
 * Functions for managing DNS
 ******************************************************************************/

func (h *host) register() error {
	err := h.registerCdn()
	if err != nil {
		return err
	}
	if h.cfrDist != nil && h.cfrDist.Status == "Deployed" {
		return h.registerNoCdn()
	}
	return nil
}

func (h *host) registerCdn() error {
	err := h.registerCdnHost()
	if err != nil {
		return fmt.Errorf("Unable to register host: %v", err)
	}
	err = h.registerToCdnRotations()
	if err != nil {
		return fmt.Errorf("Unable to register rotations: %v", err)
	}
	return nil
}

func (h *host) registerNoCdn() error {
	err := h.registerNoCdnHost()
	if err != nil {
		return fmt.Errorf("Unable to register no-CDN entry for host: %v", err)
	}
	err = h.registerToNoCdnRotations()
	if err != nil {
		return fmt.Errorf("Unable to register no-CDN rotations: %v", err)
	}
	return nil
}

func (h *host) registerCdnHost() error {
	if h.cdnRecord != nil {
		log.Tracef("Host already registered, no need to re-register: %v", h)
		return nil
	}
	log.Debugf("Registering %v", h)

	rec, err := cflutil.Register(h.name, h.ip)
	if err == nil || isDuplicateError(err) {
		h.cdnRecord = rec
		err = nil
	}
	return err
}

func (h *host) registerNoCdnHost() error {
	if h.noCdnRecord != nil {
		log.Tracef("No-CDN host already registered, no need to re-register: %v", h)
		return nil
	}
	log.Debugf("Registering no-CDN entry for %v", h)

	rec, err := cflutil.Register(noCdnPrefix+h.name, h.ip)
	if err == nil || isDuplicateError(err) {
		h.noCdnRecord = rec
		err = nil
	}
	return err
}

func (h *host) registerToCdnRotations() error {
	for _, group := range h.cdnGroups {
		err := group.register(h)
		if err != nil && !isDuplicateError(err) {
			return err
		}
	}
	return nil
}

func (h *host) registerToNoCdnRotations() error {
	for _, group := range h.noCdnGroups {
		err := group.register(h)
		if err != nil && !isDuplicateError(err) {
			return err
		}
	}
	return nil
}

func (h *host) deregister() {
	h.deregisterCdnHost()
	h.deregisterNoCdnHost()
	h.deregisterFromRotations()
}

func (h *host) deregisterCdnHost() {
	if h.cdnRecord == nil {
		log.Debugf("Host not registered, no need to deregister: %v", h)
		return
	}

	if h.isFallback() {
		log.Debugf("Currently not deregistering fallbacks like %v", h)
		return
	}

	log.Debugf("Deregistering %v", h)
	h.doDeregisterCdnHost()
}

func (h *host) deregisterNoCdnHost() {
	if h.noCdnRecord == nil {
		log.Debugf("Host no-CDN entry not registered, no need to deregister: %v", h)
		return
	}

	if h.isFallback() {
		log.Debugf("Currently not deregistering fallbacks like %v", h)
		return
	}

	log.Debugf("Deregistering no-CDN entry for %v", h)
	h.doDeregisterNoCdnHost()
}

func (h *host) doDeregisterCdnHost() {
	err := cflutil.DestroyRecord(h.cdnRecord)
	if err != nil {
		log.Errorf("Unable to deregister host %v: %v", h, err)
		return
	}

	h.cdnRecord = nil
}

func (h *host) doDeregisterNoCdnHost() {
	err := cflutil.DestroyRecord(h.noCdnRecord)
	if err != nil {
		log.Errorf("Unable to deregister host %v: %v", h, err)
		return
	}

	h.noCdnRecord = nil
}

func (h *host) deregisterFromRotations() {
	for _, group := range h.cdnGroups {
		group.deregister(h)
	}
	for _, group := range h.noCdnGroups {
		group.deregister(h)
	}
}

func (h *host) fullName() string {
	return h.name + "." + *cfldomain
}

func (h *host) isFallback() bool {
	return isCdnFallback(h.name)
}

func (h *host) isAbleToProxy() (bool, bool, error) {
	// Check whether or not we can proxy a few times
	var lastErr error
	for i := 0; i < proxyAttempts; i++ {
		success, connectionRefused, err := h.doIsAbleToProxy()
		if err != nil {
			log.Debugf("Error testing %v: %v", h, err.Error())
		}
		lastErr = err
		if success || connectionRefused {
			// If we've succeeded, or our connection was flat-out refused, don't
			// bother trying to proxy again

			if success {
				// Make sure that the server is reporting the correct host name for sticky
				// routing.
				h.reportedHostMutex.Lock()
				defer h.reportedHostMutex.Unlock()
				if h.reportedHost != h.fullName() {
					success = false
					lastErr := fmt.Errorf("%v is reporting an unexpected host %v", h, h.reportedHost)
					log.Error(lastErr.Error())
				}
			}

			return success, connectionRefused, lastErr
		}
	}
	return false, false, lastErr
}

func (h *host) doIsAbleToProxy() (bool, bool, error) {
	// First just try a plain TCP connection. This is useful because the
	// underlying TCP-level error is consumed in the flashlight layer, and we
	// need that to be accessible on the client side in the logic for deciding
	// whether or not to display the port mapping message.
	addr := h.ip + ":443"
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		err2 := fmt.Errorf("Unable to connect to %v: %v", addr, err)
		return false, strings.Contains(err.Error(), "connection refused"), err2
	}
	conn.Close()

	// Now actually try to proxy an http request
	site := testSites[rand.Intn(len(testSites))]
	resp, err := h.proxiedClient.Head("http://" + site)
	if err != nil {
		return false, false, fmt.Errorf("Unable to make proxied HEAD request to %v: %v", site, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 301 {
		err2 := fmt.Errorf("Proxying to %v via %v returned unexpected status %d,", site, h.ip, resp.StatusCode)
		return false, false, err2
	}

	return true, false, nil
}

func isDuplicateError(err error) bool {
	return strings.Contains(err.Error(), "The record already exists.")
}