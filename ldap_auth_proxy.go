package main

import (
	"encoding/base64"
	"fmt"
	"github.com/jtblin/go-ldap-client"
	log "github.com/sirupsen/logrus"
	"net/http"
	"strings"
)

// LDAPAuthProxy - a struct that represent auth proxy internal configuration
type LDAPAuthProxy struct {
	RobotsPath string
	PingPath   string
	AlivePath  string
	SignInPath string
	AuthPath   string

	SignInMessage string

	LDAPClient     *ldap.LDAPClient
	HeadersMap     map[string]string
	GroupHeader    string
	RedirectQueryAttribute string

	serveMux http.Handler
}

// NewLDAPAuthProxy - create new LDAP auth proxy
func NewLDAPAuthProxy(c *Config) (*LDAPAuthProxy, error) {

	l, err := createLDAPClient(c)

	if err != nil {
		return nil, err
	}

	mux, err := NewMux(c)

	if err != nil {
		return nil, err
	}

	p := &LDAPAuthProxy{
		RobotsPath:    "/robots.txt",
		PingPath:      "/ping",
		AlivePath:     "/alive",
		SignInPath:    c.URLPathSignIn,
		AuthPath:      c.URLPathAuth,
		SignInMessage: c.MessageAuthRequired,

		LDAPClient:  l,
		HeadersMap:  c.HeadersMap,
		GroupHeader: c.GroupHeader,
		RedirectQueryAttribute: c.RedirectQueryAttribute,

		serveMux: mux,
	}

	return p, nil
}

type loggedResponseWriter struct {
	http.ResponseWriter
	status int
}

func (l *loggedResponseWriter) WriteHeader(status int) {
	l.status = status
	l.ResponseWriter.WriteHeader(status)
}

func (p *LDAPAuthProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	lw := &loggedResponseWriter{ResponseWriter: w}
	log.Debugf(">>> %s %s", r.Method, r.URL)

	switch r.URL.Path {
	case p.AuthPath:
		p.AuthenticateOnly(lw, r)
		break
	case p.RobotsPath:
		p.RobotsTxt(lw, r)
		break
	case p.PingPath:
		p.PingPage(lw, r)
		break
	case p.AlivePath:
		p.AlivePage(lw, r)
		break
	case p.SignInPath:
		p.SignIn(lw, r)
		break
	default:
		p.Proxy(lw, r)
		break
	}

	// TODO: log username and whether status comes from proxy (e.g., add * to denote that it's a status from proxy
	log.Debugf("<<< %s %s %d", r.Method, r.URL, lw.status)
}

// RobotsTxt - serve robots.txt file
func (p *LDAPAuthProxy) RobotsTxt(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "User-agent: *\nDisallow: /")
}

// PingPage - check that app is up and running
func (p *LDAPAuthProxy) PingPage(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "OK")
}

// AlivePage - check that app and running and can serve requests properly
func (p *LDAPAuthProxy) AlivePage(w http.ResponseWriter, r *http.Request) {
	defer p.LDAPClient.Close()

	err := p.LDAPClient.Connect()

	if err != nil {
		traceWarning(w, fmt.Sprintf("Failed to connect: %s", err.Error()))
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, "ERROR")
		return
	}

	err = p.LDAPClient.Conn.Bind(p.LDAPClient.BindDN, p.LDAPClient.BindPassword)

	if err != nil {
		traceWarning(w, fmt.Sprintf("Failed to bind: %s (%s, %s)", err.Error(), p.LDAPClient.BindDN, p.LDAPClient.BindPassword))
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "ERROR")
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "OK")
}

// SignIn - serve sign in page
func (p *LDAPAuthProxy) SignIn(w http.ResponseWriter, r *http.Request) {
	status := p.authenticate(w, r)

	if status != http.StatusAccepted {
		sendError(w, status)
	} else {
		redirect := r.URL.Query().Get(p.RedirectQueryAttribute)
		http.Redirect(w, r, r.Host+redirect, http.StatusFound)
	}
}

// AuthenticateOnly - serve auth-only endpoint
func (p *LDAPAuthProxy) AuthenticateOnly(w http.ResponseWriter, r *http.Request) {
	status := p.authenticate(w, r)
	if status != http.StatusAccepted {
		sendError(w, status)
	} else {
		w.WriteHeader(status)
	}
}

// Proxy - proxy incoming request to the upstream
func (p *LDAPAuthProxy) Proxy(w http.ResponseWriter, r *http.Request) {
	status := p.authenticate(w, r)

	if status != http.StatusAccepted {
		sendError(w, status)
		return
	}

	p.serveMux.ServeHTTP(w, r)
}

// authenticate - authenticate user from request
func (p *LDAPAuthProxy) authenticate(w http.ResponseWriter, r *http.Request) int {
	s := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
	if len(s) != 2 {
		traceDebug(w, "Malformed auth header value")
		return http.StatusUnauthorized
	}

	b, err := base64.StdEncoding.DecodeString(s[1])
	if err != nil {
		traceWarning(w, fmt.Sprintf("Failed to decode HTTP Authorisation header value: %s", err))
		return http.StatusBadRequest
	}

	pair := strings.SplitN(string(b), ":", 2)
	if len(pair) != 2 {
		traceWarning(w, fmt.Sprintf("Bad HTTP Authorisation header value: %s", string(b)))
		return http.StatusBadRequest
	}

	if pair[0] == "" || pair[1] == "" {
		traceWarning(w, fmt.Sprintf("Only name/password authentication is supported (username and/or password are empty)"))
		return http.StatusUnauthorized
	}

	filterGroups := []string{"*"}

	if p.GroupHeader != "" {
		if r.Header.Get(p.GroupHeader) == "" {
			traceDebug(w, "No filterGroups header or empty filterGroups value when it required by configuration")
			return http.StatusBadRequest
		}

		filterString := r.Header.Get(p.GroupHeader)
		filterGroups = extractFilterGroups(filterString)

		if len(filterGroups) < 1 {
			traceWarning(w, fmt.Sprintf("Bad groups filter string: %s", filterString))
			return http.StatusBadGateway
		}
	}

	err = p.LDAPClient.Connect()

	if err != nil {
		traceWarning(w, fmt.Sprintf("Failed to connect: %s", err.Error()))
		return http.StatusBadGateway
	}

	defer p.LDAPClient.Close()

	authenticated, attributes, err := p.LDAPClient.Authenticate(pair[0], pair[1])

	if err != nil {
		traceWarning(w, fmt.Sprintf("Failed to authenticate: %s", err.Error()))
		return http.StatusUnauthorized
	}

	if !authenticated {
		traceDebug(w, "Not authenticated by LDAP")
		return http.StatusUnauthorized
	}

	// Special case
	if len(filterGroups) > 0 && filterGroups[0] == "*" {
		writeAttributes(p.HeadersMap, attributes, w)
		return http.StatusAccepted
	}

	groupsOfUser, err := p.LDAPClient.GetGroupsOfUser(pair[0])

	if err != nil {
		traceError(w, fmt.Sprintf("Failed to get user groups: %s", err.Error()))
		return http.StatusUnauthorized
	}

	for _, gUser := range groupsOfUser {
		for _, gFilter := range filterGroups {
			if gUser == gFilter {
				writeAttributes(p.HeadersMap, attributes, w)
				return http.StatusAccepted
			}
		}
	}

	traceDebug(w, "Not authorized as per LDAP groups")
	return http.StatusForbidden
}

// writeAttributes - map LDAP attributes back to HTTP headers and write them
func writeAttributes(headers map[string]string, attributes map[string]string, w http.ResponseWriter) {
	log.Debugf("Headers: %+v, Attributes: %+v", headers, attributes)
	for h, a := range headers {
		if !strings.HasPrefix(h, "X-") && !strings.HasPrefix(h, "x-") {
			h = "X-" + h
		}

		w.Header().Set(h, attributes[a])
	}
}

func traceDebug(w http.ResponseWriter, h string) {
	if log.GetLevel() < log.DebugLevel {
		return
	}

	log.Debug(h)
	w.Header().Add("X-LdapAuth-Trace", h)
}

func traceWarning(w http.ResponseWriter, h string) {
	if log.GetLevel() < log.DebugLevel {
		return
	}

	log.Warning(h)
	w.Header().Add("X-LdapAuth-Trace", h)
}

func traceError(w http.ResponseWriter, h string) {
	if log.GetLevel() < log.DebugLevel {
		return
	}

	log.Warning(h)
	w.Header().Add("X-LdapAuth-Trace", h)
}

func extractFilterGroups(filterString string) []string {
	var filterGroups []string

	rawGroup := strings.Split(filterString, ",")

	for _, g := range rawGroup {
		g = strings.TrimSpace(g)

		if "*" == g {
			// special case, we don't need any other filters with wildcard
			return []string{"*"}
		}

		if len(g) > 1 {
			filterGroups = append(filterGroups, g)
		}
	}

	return filterGroups
}

func sendError(w http.ResponseWriter, status int) {
	if http.StatusUnauthorized == status {
		w.Header().Set("WWW-authenticate", fmt.Sprintf(`Basic realm="%s"`, "Authorization required"))
	}
	http.Error(w, http.StatusText(status), status)
}
