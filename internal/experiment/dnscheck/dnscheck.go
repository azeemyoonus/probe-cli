// Package dnscheck contains the DNS check experiment.
//
// See https://github.com/ooni/spec/blob/master/nettests/ts-028-dnscheck.md.
package dnscheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ooni/probe-cli/v3/internal/experiment/urlgetter"
	"github.com/ooni/probe-cli/v3/internal/legacy/netx"
	"github.com/ooni/probe-cli/v3/internal/legacy/tracex"
	"github.com/ooni/probe-cli/v3/internal/model"
	"github.com/ooni/probe-cli/v3/internal/runtimex"
	"github.com/ooni/probe-cli/v3/internal/targetloading"
)

const (
	testName      = "dnscheck"
	testVersion   = "0.9.2"
	defaultDomain = "example.org"
)

// Endpoints keeps track of repeatedly measured endpoints.
type Endpoints struct {
	WaitTime  time.Duration
	count     *atomic.Int64
	nextVisit map[string]time.Time
	mu        sync.Mutex
}

func (e *Endpoints) maybeSleep(resolverURL string, logger model.Logger) {
	if e == nil {
		return
	}
	defer e.mu.Unlock()
	e.mu.Lock()
	nextTime, found := e.nextVisit[resolverURL]
	now := time.Now()
	if !found || now.After(nextTime) {
		return
	}
	sleepTime := nextTime.Sub(now)
	if e.count == nil {
		e.count = &atomic.Int64{}
	}
	e.count.Add(1)
	logger.Infof("waiting %v before testing %s again", sleepTime, resolverURL)
	time.Sleep(sleepTime)
}

func (e *Endpoints) maybeRegister(resolverURL string) {
	if e != nil && !strings.HasPrefix(resolverURL, "udp://") {
		defer e.mu.Unlock()
		e.mu.Lock()
		if e.nextVisit == nil {
			e.nextVisit = make(map[string]time.Time)
		}
		waitTime := 180 * time.Second
		if e.WaitTime > 0 {
			waitTime = e.WaitTime
		}
		e.nextVisit[resolverURL] = time.Now().Add(waitTime)
	}
}

// Config contains the experiment's configuration.
type Config struct {
	DefaultAddrs  string `json:"default_addrs" ooni:"default addresses for domain"`
	Domain        string `json:"domain" ooni:"domain to resolve using the specified resolver"`
	HTTP3Enabled  bool   `json:"http3_enabled" ooni:"use http3 instead of http/1.1 or http2"`
	HTTPHost      string `json:"http_host" ooni:"force using specific HTTP Host header"`
	TLSServerName string `json:"tls_server_name" ooni:"force TLS to using a specific SNI in Client Hello"`
	TLSVersion    string `json:"tls_version" ooni:"Force specific TLS version (e.g. 'TLSv1.3')"`
}

// TestKeys contains the results of the dnscheck experiment.
type TestKeys struct {
	DefaultAddrs     string                        `json:"x_default_addrs"`
	Domain           string                        `json:"domain"`
	HTTP3Enabled     bool                          `json:"x_http3_enabled,omitempty"`
	HTTPHost         string                        `json:"x_http_host,omitempty"`
	TLSServerName    string                        `json:"x_tls_server_name,omitempty"`
	TLSVersion       string                        `json:"x_tls_version,omitempty"`
	Residual         bool                          `json:"x_residual"`
	Bootstrap        *urlgetter.TestKeys           `json:"bootstrap"`
	BootstrapFailure *string                       `json:"bootstrap_failure"`
	Lookups          map[string]urlgetter.TestKeys `json:"lookups"`
}

// Measurer performs the measurement.
type Measurer struct {
	Endpoints *Endpoints
}

// ExperimentName implements model.ExperimentSession.ExperimentName
func (m *Measurer) ExperimentName() string {
	return testName
}

// ExperimentVersion implements model.ExperimentSession.ExperimentVersion
func (m *Measurer) ExperimentVersion() string {
	return testVersion
}

// The following errors may be returned by this experiment. Of course these
// errors are in addition to any other errors returned by the low level packages
// that are used by this experiment to implement its functionality.
var (
	ErrInputRequired        = targetloading.ErrInputRequired
	ErrInvalidURL           = errors.New("the input URL is invalid")
	ErrUnsupportedURLScheme = errors.New("unsupported URL scheme")
)

// Run implements model.ExperimentSession.Run
func (m *Measurer) Run(ctx context.Context, args *model.ExperimentArgs) error {
	_ = args.Callbacks
	measurement := args.Measurement
	sess := args.Session

	// 0. obtain the richer input target, config, and input or panic
	if args.Target == nil {
		return ErrInputRequired
	}
	target := args.Target.(*Target)
	config, input := target.Options, target.URL
	sess.Logger().Infof("dnscheck: using richer input: %+v %+v", config, input)

	// 1. fill the measurement with test keys
	tk := new(TestKeys)
	tk.Lookups = make(map[string]urlgetter.TestKeys)
	measurement.TestKeys = tk
	urlgetter.RegisterExtensions(measurement)

	// 2. select the domain to resolve or use default and, while there, also
	// ensure that we register all the other options we're using.
	domain := config.Domain
	if domain == "" {
		domain = defaultDomain
	}
	tk.DefaultAddrs = config.DefaultAddrs
	tk.Domain = domain
	tk.HTTP3Enabled = config.HTTP3Enabled
	tk.HTTPHost = config.HTTPHost
	tk.TLSServerName = config.TLSServerName
	tk.TLSVersion = config.TLSVersion
	tk.Residual = m.Endpoints != nil

	// 3. parse the input URL describing the resolver to use
	if input == "" {
		return ErrInputRequired
	}
	URL, err := url.Parse(input)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidURL, err.Error())
	}
	switch URL.Scheme {
	case "https", "dot", "udp", "tcp":
		// all good
	default:
		return ErrUnsupportedURLScheme
	}

	// Implementation note: we must not return an error from now now. Returning an
	// error means that we don't have a measurement to submit.

	// 4. possibly expand a domain to a list of IP addresses.
	//
	// Implementation note: because the resolver we constructed also deals
	// with IP addresses successfully, we just get back the IPs when we are
	// passing as input an IP address rather than a domain name.
	begin := measurement.MeasurementStartTimeSaved
	evsaver := new(tracex.Saver)
	resolver := netx.NewResolver(netx.Config{
		BogonIsError: true,
		Logger:       sess.Logger(),
		Saver:        evsaver,
	})
	addrs, err := m.lookupHost(ctx, URL.Hostname(), resolver)
	queries := tracex.NewDNSQueriesList(begin, evsaver.Read())
	tk.BootstrapFailure = tracex.NewFailure(err)
	if len(queries) > 0 {
		// We get no queries in case we are resolving an IP address, since
		// the address resolver doesn't generate events
		tk.Bootstrap = &urlgetter.TestKeys{Queries: queries}
	}

	// 5. merge default addresses for the domain with the ones that
	// we did discover here and measure them all.
	allAddrs := make(map[string]bool)
	for _, addr := range addrs {
		allAddrs[addr] = true
	}
	for _, addr := range strings.Split(config.DefaultAddrs, " ") {
		if addr != "" {
			allAddrs[addr] = true
		}
	}

	// 6. determine all the domain lookups we need to perform
	const maxParallelism = 10
	parallelism := maxParallelism
	if parallelism > len(allAddrs) {
		parallelism = len(allAddrs)
	}
	var inputs []urlgetter.MultiInput
	multi := urlgetter.Multi{Begin: begin, Parallelism: parallelism, Session: sess}
	for addr := range allAddrs {
		inputs = append(inputs, urlgetter.MultiInput{
			Config: urlgetter.Config{
				DNSHTTPHost:      config.httpHost(URL.Host),
				DNSTLSServerName: config.tlsServerName(URL.Hostname()),
				DNSTLSVersion:    config.TLSVersion,
				HTTP3Enabled:     config.HTTP3Enabled,
				RejectDNSBogons:  true, // bogons are errors in this context
				ResolverURL:      makeResolverURL(URL, addr),
				Timeout:          15 * time.Second,
			},
			Target: fmt.Sprintf("dnslookup://%s", domain), // urlgetter wants a URL
		})
	}

	// 7. make sure we don't test the same endpoint too frequently
	// because this may cause residual censorship.
	for _, input := range inputs {
		resolverURL := input.Config.ResolverURL
		m.Endpoints.maybeSleep(resolverURL, sess.Logger())
	}

	// 8. perform all the required resolutions
	for output := range Collect(ctx, multi, inputs, sess.Logger()) {
		resolverURL := output.Input.Config.ResolverURL
		tk.Lookups[resolverURL] = output.TestKeys
		m.Endpoints.maybeRegister(resolverURL)
	}
	return nil
}

func (m *Measurer) lookupHost(ctx context.Context, hostname string, r model.Resolver) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return r.LookupHost(ctx, hostname)
}

// httpHost returns the configured HTTP host, if set, otherwise
// it will return the host provide as argument.
func (c *Config) httpHost(httpHost string) string {
	if c.HTTPHost != "" {
		return c.HTTPHost
	}
	return httpHost
}

// tlsServerName is like httpHost for the TLS server name.
func (c *Config) tlsServerName(tlsServerName string) string {
	if c.TLSServerName != "" {
		return c.TLSServerName
	}
	return tlsServerName
}

// Collect prints on the output channel the result of running dnscheck
// on every provided input. It closes the output channel when done.
func Collect(ctx context.Context, multi urlgetter.Multi, inputs []urlgetter.MultiInput,
	logger model.Logger) <-chan urlgetter.MultiOutput {
	outputch := make(chan urlgetter.MultiOutput)
	expect := len(inputs)
	inputch := multi.Run(ctx, inputs)
	go func() {
		var count int
		defer close(outputch)
		for count < expect {
			entry := <-inputch
			count++
			logger.Infof("dnscheck: measure %s: %+v", entry.Input.Config.ResolverURL,
				model.ErrorToStringOrOK(entry.Err))
			outputch <- entry
		}
	}()
	return outputch
}

// makeResolverURL rewrites the input URL to replace the domain in
// the input URL with the given addr. When the input URL already contains
// an addr, this operation will return the same URL.
func makeResolverURL(URL *url.URL, addr string) string {
	// 1. determine the hostname in the resulting URL
	hostname := URL.Hostname()
	if net.ParseIP(hostname) == nil {
		hostname = addr
	}
	// 2. adjust hostname if we also have a port
	if hasPort := URL.Port() != ""; hasPort {
		_, port, err := net.SplitHostPort(URL.Host)
		// We say this cannot fail because we already parsed the URL to validate
		// its scheme and hence the URL hostname should be well formed.
		runtimex.PanicOnError(err, "net.SplitHostPort should not fail here")
		hostname = net.JoinHostPort(hostname, port)
	} else if idx := strings.Index(addr, ":"); idx >= 0 {
		// Make sure an IPv6 address hostname without a port is properly
		// quoted to avoid breaking the URL parser down the line.
		hostname = "[" + addr + "]"
	}
	// 3. reassemble the URL
	return (&url.URL{
		Scheme:   URL.Scheme,
		Host:     hostname,
		Path:     URL.Path,
		RawQuery: URL.RawQuery,
	}).String()
}

// NewExperimentMeasurer creates a new ExperimentMeasurer.
func NewExperimentMeasurer() model.ExperimentMeasurer {
	return &Measurer{
		Endpoints: nil, // disabled by default
	}
}
