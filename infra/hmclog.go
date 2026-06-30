package infra

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/IBM/shiftlaunch/logger"
)

// logMu protects the raw file descriptor from concurrent interleaved writes.
var logMu sync.Mutex

// sessionRegex targets the HMC token inside XML response payloads as a catch-all
// safety net when the token is not yet present in any header (e.g. login response).
var sessionRegex = regexp.MustCompile(`(?i)(<X-API-Session>)(.*?)(</X-API-Session>)`)

// debugRoundTripper is an http.RoundTripper that logs every HMC request and
// response through shiftlaunch's logger. The base transport (TLS) is provided
// by hmc.NewRestClientWithTransport via the TransportWrapper factory.
type debugRoundTripper struct {
	base   http.RoundTripper
	logger *logger.Logger
}

func (d *debugRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Console + file: compact one-liner.
	d.logger.Debug("[HMC →] " + req.Method + " " + req.URL.String())

	resp, err := d.base.RoundTrip(req)
	if err != nil {
		d.logger.Debug("[HMC ✗] " + req.Method + " " + req.URL.String() + " — " + err.Error())
		return nil, err
	}

	// Read and buffer the body so the caller can still consume it.
	// Check the read error immediately — a partial body must not be logged or returned.
	body, err := io.ReadAll(resp.Body)
	if closeErr := resp.Body.Close(); closeErr != nil {
		d.logger.Debug("[HMC ✗] body close: " + closeErr.Error())
	}
	if err != nil {
		return nil, err
	}

	// Restore the body for the caller before any further processing.
	resp.Body = io.NopCloser(bytes.NewReader(body))

	// Console + file: one-liner with status (no body — avoids charmbracelet truncation).
	d.logger.Debug("[HMC ←] " + resp.Status + " " + req.URL.String())

	// File only: full untruncated body, written directly to bypass charmbracelet.
	// logMu gates the raw *os.File so parallel goroutines cannot interleave writes.
	bodyStr := maskSession(req, resp, string(body))
	logMu.Lock()
	fmt.Fprintf(d.logger.FileOnly(),
		"%s DEBU [HMC body] %s %s\n%s\n",
		time.Now().Format("2006/01/02 15:04:05"), resp.Status, req.URL.String(), bodyStr,
	)
	logMu.Unlock()
	return resp, nil
}

// maskSession redacts the X-API-Session token from a log payload using three
// complementary strategies:
//  1. Request header — covers all authenticated API calls.
//  2. Response header — covers the login response where the token is first issued.
//  3. Regex scrub — removes the token directly from the XML body as an absolute
//     safety net, regardless of header availability.
func maskSession(req *http.Request, resp *http.Response, payload string) string {
	// 1. Redact token carried on outgoing requests (standard authenticated calls).
	if token := req.Header.Get("X-API-Session"); token != "" {
		payload = strings.ReplaceAll(payload, token, "***[REDACTED]***")
	}

	// 2. Redact token returned on login — present in the response header, not the request.
	if resp != nil {
		if token := resp.Header.Get("X-API-Session"); token != "" {
			payload = strings.ReplaceAll(payload, token, "***[REDACTED]***")
		}
	}

	// 3. Hard scrub any remaining <X-API-Session>…</X-API-Session> element in the XML body.
	payload = sessionRegex.ReplaceAllString(payload, "${1}***[REDACTED]***${3}")

	return payload
}

// HMCDebugTransport returns an http.RoundTripper middleware factory that
// wraps the SDK's TLS transport with request/response logging. Pass the result
// to hmc.RestClient.WithTransport:
//
//	base := hmc.NewRestClient(ip).WithTLSInsecure()
//	client := base.WithTransport(infra.HMCDebugTransport(log)(base.HTTPTransport()))
func HMCDebugTransport(log *logger.Logger) func(http.RoundTripper) http.RoundTripper {
	return func(base http.RoundTripper) http.RoundTripper {
		return &debugRoundTripper{base: base, logger: log}
	}
}
