// Package traefik_modsecurity_plugin a modsecurity plugin.
package traefik_modsecurity_plugin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"time"
)

// Net client is a custom client to timeout after 2 seconds if the service is not ready
var httpClient = &http.Client{
	Timeout: time.Second * 2,
}

// Config the plugin configuration.
type Config struct {
	ModSecurityUrl   string `json:"modSecurityUrl,omitempty"`
	MaxBodySize      int64  `json:"maxBodySize"`
	InterruptOnError bool   `json:"InterruptOnError"`
	Ignore500Error   bool   `json:"Ignore500Error"`
}

// CreateConfig creates the default plugin configuration.
func CreateConfig() *Config {
	return &Config{
		// Safe default: if the max body size was not specified, use 10MB
		// Note that this will break any file upload with files > 10MB. Hopefully
		// the user will configure this parameter during the installation.
		MaxBodySize:      10 * 1024 * 1024,
		InterruptOnError: true,
		Ignore500Error:   false,
	}
}

// Modsecurity a Modsecurity plugin.
type Modsecurity struct {
	next             http.Handler
	modSecurityUrl   string
	maxBodySize      int64
	interruptOnError bool
	ignore500Error   bool
	name             string
	logger           *log.Logger
}

// New created a new Modsecurity plugin.
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if len(config.ModSecurityUrl) == 0 {
		return nil, fmt.Errorf("modSecurityUrl cannot be empty")
	}

	return &Modsecurity{
		modSecurityUrl:   config.ModSecurityUrl,
		maxBodySize:      config.MaxBodySize,
		interruptOnError: config.InterruptOnError,
		ignore500Error:   config.Ignore500Error,
		next:             next,
		name:             name,
		logger:           log.New(os.Stdout, "", log.LstdFlags),
	}, nil
}

func (a *Modsecurity) ServeHTTP(rw http.ResponseWriter, req *http.Request) {

	defer func() {
		if r := recover(); r != nil {
			a.handleError(rw, req, fmt.Sprintf("Panic. Error: %s", r), http.StatusBadGateway)
			return
		}
	}()

	// Websocket not supported
	if isWebsocket(req) {
		a.next.ServeHTTP(rw, req)
		return
	}

	// we need to buffer the body if we want to read it here and send it
	// in the request.
	body, err := ioutil.ReadAll(http.MaxBytesReader(rw, req.Body, a.maxBodySize))
	if err != nil {
		if err.Error() == "http: request body too large" {
			a.handleError(rw, req, fmt.Sprintf("body max limit reached: %s", err.Error()), http.StatusRequestEntityTooLarge)
		} else {
			a.handleError(rw, req, fmt.Sprintf("fail to read incoming request: %s", err.Error()), http.StatusBadGateway)
		}
		return
	}

	// you can reassign the body if you need to parse it as multipart
	req.Body = ioutil.NopCloser(bytes.NewReader(body))

	// create a new url from the raw RequestURI sent by the client
	url := fmt.Sprintf("%s%s", a.modSecurityUrl, req.RequestURI)

	proxyReq, err := http.NewRequest(req.Method, url, bytes.NewReader(body))

	if err != nil {
		a.handleError(rw, req, fmt.Sprintf("fail to prepare forwarded request: %s", err.Error()), http.StatusBadGateway)
		return
	}

	// We may want to filter some headers, otherwise we could just use a shallow copy
	// proxyReq.Header = req.Header
	proxyReq.Header = make(http.Header)
	for h, val := range req.Header {
		proxyReq.Header[h] = val
	}

	resp, err := httpClient.Do(proxyReq)
	if err != nil {
		a.handleError(rw, req, fmt.Sprintf("fail to send HTTP request to modsec: %s", err.Error()), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		if resp.StatusCode >= 500 {
			a.logger.Print("OWASP 500 error. Request ", req)
			a.logger.Print("OWASP 500 error. Response ", resp)
		}
		if resp.StatusCode < 500 || !a.ignore500Error {
			forwardResponse(resp, rw)
			return
		}
	}

	a.next.ServeHTTP(rw, req)
}

func isWebsocket(req *http.Request) bool {
	for _, header := range req.Header["Upgrade"] {
		if header == "websocket" {
			return true
		}
	}
	return false
}

func forwardResponse(resp *http.Response, rw http.ResponseWriter) {
	// copy headers
	for k, vv := range resp.Header {
		for _, v := range vv {
			rw.Header().Set(k, v)
		}
	}
	// copy status
	rw.WriteHeader(resp.StatusCode)
	// copy body
	io.Copy(rw, resp.Body)
}

func (a *Modsecurity) handleError(rw http.ResponseWriter, req *http.Request, errorMessage string, code int) {
	a.logger.Printf(errorMessage)
	a.logger.Print("ModSecurity::handleError Request: ", req)
	if a.interruptOnError {
		a.logger.Print("ModSecurity::handleError [Interrupt]")
		http.Error(rw, "", code)
	} else {
		a.logger.Print("ModSecurity::handleError [Continue]")
		a.next.ServeHTTP(rw, req)
	}
}
