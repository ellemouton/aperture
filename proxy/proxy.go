package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"regexp"
	"strconv"
	"strings"

	"github.com/lightninglabs/aperture/auth"
	"github.com/lightninglabs/aperture/lsat"
	"google.golang.org/grpc/codes"
)

const (
	// formatPattern is the pattern in which the request log will be
	// printed. This is loosely oriented on the apache log format.
	// An example entry would look like this:
	// 2019-11-09 04:07:55.072 [INF] PRXY: 66.249.69.89 - -
	// "GET /availability/v1/btc.json HTTP/1.1" "" "Mozilla/5.0 ..."
	formatPattern  = "- - \"%s %s %s\" \"%s\" \"%s\""
	hdrContentType = "Content-Type"
	hdrTypeGrpc    = "application/grpc"
)

// Proxy is a HTTP, HTTP/2 and gRPC handler that takes an incoming request,
// uses its authenticator to validate the request's headers, and either returns
// a challenge to the client or forwards the request to another server and
// proxies the response back to the client.
type Proxy struct {
	proxyBackend  *httputil.ReverseProxy
	staticServer  http.Handler
	authenticator auth.Authenticator
	services      []*Service
}

// New returns a new Proxy instance that proxies between the services specified,
// using the auth to validate each request's headers and get new challenge
// headers if necessary.
func New(auth auth.Authenticator, services []*Service, serveStatic bool,
	staticRoot string) (*Proxy, error) {

	// By default the static file server only returns 404 answers for
	// security reasons. Serving files from the staticRoot directory has to
	// be enabled intentionally.
	staticServer := http.NotFoundHandler()
	if serveStatic {
		if len(strings.TrimSpace(staticRoot)) == 0 {
			return nil, fmt.Errorf("staticroot cannot be empty, " +
				"must contain path to directory that " +
				"contains index.html")
		}
		staticServer = http.FileServer(http.Dir(staticRoot))
	}

	proxy := &Proxy{
		staticServer:  staticServer,
		authenticator: auth,
		services:      services,
	}
	err := proxy.UpdateServices(services)
	if err != nil {
		return nil, err
	}

	return proxy, nil
}

// ServeHTTP checks a client's headers for appropriate authorization and either
// returns a challenge or forwards their request to the target backend service.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Parse and log the remote IP address. We also need the parsed IP
	// address for the freebie count.
	remoteIP, prefixLog := NewRemoteIPPrefixLog(log, r.RemoteAddr)
	logRequest := func() {
		prefixLog.Infof(formatPattern, r.Method, r.RequestURI, r.Proto,
			r.Referer(), r.UserAgent())
	}
	defer logRequest()

	// For OPTIONS requests we only need to set the CORS headers, not serve
	// any content;
	if r.Method == "OPTIONS" {
		addCorsHeaders(w.Header())
		sendDirectResponse(w, r, http.StatusOK, "")
		return
	}

	// Requests that can't be matched to a service backend will be
	// dispatched to the static file server. If the file exists in the
	// static file folder it will be served, otherwise the static server
	// will return a 404 for us.
	target, ok := matchService(r, p.services)
	if !ok {
		prefixLog.Debugf("Dispatching request %s to static file "+
			"server.", r.URL.Path)
		p.staticServer.ServeHTTP(w, r)
		return
	}

	// Determine auth level required to access service and dispatch request
	// accordingly.
	authLevel := target.AuthRequired(r)
	switch {
	case authLevel.IsOn():
		if !p.authenticator.Accept(&r.Header, target.Name) {
			prefixLog.Infof("Authentication failed. Sending 402.")
			p.handlePaymentRequired(w, r, target.Name, target.Price)
			return
		}

	case authLevel.IsFreebie():
		// We only need to respect the freebie counter if the user
		// is not authenticated at all.
		if !p.authenticator.Accept(&r.Header, target.Name) {
			ok, err := target.freebieDb.CanPass(r, remoteIP)
			if err != nil {
				prefixLog.Errorf("Error querying freebie db: "+
					"%v", err)
				sendDirectResponse(
					w, r, http.StatusInternalServerError,
					"freebie DB failure",
				)
				return
			}
			if !ok {
				p.handlePaymentRequired(w, r, target.Name, target.Price)
				return
			}
			_, err = target.freebieDb.TallyFreebie(r, remoteIP)
			if err != nil {
				prefixLog.Errorf("Error updating freebie db: "+
					"%v", err)
				sendDirectResponse(
					w, r, http.StatusInternalServerError,
					"freebie DB failure",
				)
				return
			}
		}
	}

	// If we got here, it means everything is OK to pass the request to the
	// service backend via the reverse proxy.
	p.proxyBackend.ServeHTTP(w, r)
}

// UpdateServices re-configures the proxy to use a new set of backend services.
func (p *Proxy) UpdateServices(services []*Service) error {
	err := prepareServices(services)
	if err != nil {
		return err
	}

	certPool, err := certPool(services)
	if err != nil {
		return err
	}
	transport := &http.Transport{
		ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{
			RootCAs:            certPool,
			InsecureSkipVerify: true,
		},
	}

	p.proxyBackend = &httputil.ReverseProxy{
		Director:  p.director,
		Transport: transport,
		ModifyResponse: func(res *http.Response) error {
			addCorsHeaders(res.Header)
			return nil
		},

		// A negative value means to flush immediately after each write
		// to the client.
		FlushInterval: -1,
	}

	return nil
}

// Close cleans up the Proxy by closing any remaining open connections.
func (p *Proxy) Close() error {
	return nil
}

// director is a method that rewrites an incoming request to be forwarded to a
// backend service.
func (p *Proxy) director(req *http.Request) {
	target, ok := matchService(req, p.services)
	if ok {
		// Rewrite address and protocol in the request so the
		// real service is called instead.
		req.Host = target.Address
		req.URL.Host = target.Address
		req.URL.Scheme = target.Protocol

		// Make sure we always forward the authorization in the correct/
		// default format so the backend knows what to do with it.
		mac, preimage, err := lsat.FromHeader(&req.Header)
		if err == nil {
			// It could be that there is no auth information because
			// none is needed for this particular request. So we
			// only continue if no error is set.
			err := lsat.SetHeader(&req.Header, mac, preimage)
			if err != nil {
				log.Errorf("could not set header: %v", err)
			}
		}

		// Now overwrite header fields of the client request
		// with the fields from the configuration file.
		for name, value := range target.Headers {
			req.Header.Add(name, value)
		}
	}
}

// certPool builds a pool of x509 certificates from the backend services.
func certPool(services []*Service) (*x509.CertPool, error) {
	cp := x509.NewCertPool()
	for _, service := range services {
		if service.TLSCertPath == "" {
			continue
		}

		b, err := ioutil.ReadFile(service.TLSCertPath)
		if err != nil {
			return nil, err
		}

		if !cp.AppendCertsFromPEM(b) {
			return nil, fmt.Errorf("credentials: failed to " +
				"append certificate")
		}
	}

	return cp, nil
}

// matchService tries to match a backend service to an HTTP request by regular
// expression matching the host and path.
func matchService(req *http.Request, services []*Service) (*Service, bool) {
	for _, service := range services {
		hostRegexp := regexp.MustCompile(service.HostRegexp)
		if !hostRegexp.MatchString(req.Host) {
			log.Tracef("Req host [%s] doesn't match [%s].",
				req.Host, hostRegexp)
			continue
		}

		if service.PathRegexp == "" {
			log.Debugf("Host [%s] matched pattern [%s] and path "+
				"expression is empty. Using service [%s].",
				req.Host, hostRegexp, service.Address)
			return service, true
		}

		pathRegexp := regexp.MustCompile(service.PathRegexp)
		if !pathRegexp.MatchString(req.URL.Path) {
			log.Tracef("Req path [%s] doesn't match [%s].",
				req.URL.Path, pathRegexp)
			continue
		}

		log.Debugf("Host [%s] matched pattern [%s] and path [%s] "+
			"matched [%s]. Using service [%s].",
			req.Host, hostRegexp, req.URL.Path, pathRegexp,
			service.Address)
		return service, true
	}
	log.Errorf("No backend service matched request [%s%s].", req.Host,
		req.URL.Path)
	return nil, false
}

// addCorsHeaders adds HTTP header fields that are required for Cross Origin
// Resource Sharing. These header fields are needed to signal to the browser
// that it's ok to allow requests to sub domains, even if the JS was served from
// the top level domain.
func addCorsHeaders(header http.Header) {
	log.Debugf("Adding CORS headers to response.")

	header.Add("Access-Control-Allow-Origin", "*")
	header.Add("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	header.Add("Access-Control-Expose-Headers", "WWW-Authenticate")
	header.Add(
		"Access-Control-Allow-Headers",
		"Authorization, Grpc-Metadata-macaroon, WWW-Authenticate",
	)
}

// handlePaymentRequired returns fresh challenge header fields and status code
// to the client signaling that a payment is required to fulfil the request.
func (p *Proxy) handlePaymentRequired(w http.ResponseWriter, r *http.Request,
	serviceName string, servicePrice int64) {

	addCorsHeaders(r.Header)

	header, err := p.authenticator.FreshChallengeHeader(r, serviceName, servicePrice)
	if err != nil {
		log.Errorf("Error creating new challenge header: %v", err)
		sendDirectResponse(
			w, r, http.StatusInternalServerError,
			"challenge failure",
		)
		return
	}

	for name, value := range header {
		w.Header().Set(name, value[0])
		for i := 1; i < len(value); i++ {
			w.Header().Add(name, value[i])
		}
	}

	sendDirectResponse(w, r, http.StatusPaymentRequired, "payment required")
}

// sendDirectResponse sends a response directly to the client without proxying
// anything to a backend. The given error is transported in a way the client can
// understand. This means, for a gRPC client it is sent as specific header
// fields.
func sendDirectResponse(w http.ResponseWriter, r *http.Request,
	statusCode int, errInfo string) {

	// Find out if the client is a normal HTTP or a gRPC client. Every gRPC
	// request should have the Content-Type header field set accordingly
	// so we can use that.
	switch {
	case strings.HasPrefix(r.Header.Get(hdrContentType), hdrTypeGrpc):
		w.Header().Set("Grpc-Status", strconv.Itoa(int(codes.Internal)))
		w.Header().Set("Grpc-Message", errInfo)
		w.WriteHeader(statusCode)

	default:
		http.Error(w, errInfo, statusCode)
	}
}
