package remote

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Address      string
	TokenFile    string
	AllowLAN     bool
	TLSCert      string
	TLSKey       string
	PublicOrigin string
}

type Server struct {
	HTTP      *http.Server
	URL       string
	TokenFile string
	hub       *Hub
	done      chan struct{}
	mu        sync.Mutex
	serveErr  error
}

func Start(cfg Config, hub *Hub) (*Server, error) {
	if hub == nil {
		return nil, ErrUnavailable
	}
	if cfg.Address == "" {
		cfg.Address = "127.0.0.1:0"
	}
	host, _, err := net.SplitHostPort(cfg.Address)
	if err != nil {
		return nil, errors.New("bind requires literal IP and port")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.IsUnspecified() || (!ip.IsLoopback() && (!ip.IsPrivate() || !cfg.AllowLAN)) {
		return nil, errors.New("bind requires loopback or explicit private LAN opt-in; wildcard/public binds refused")
	}
	useTLS := cfg.TLSCert != "" && cfg.TLSKey != ""
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") || (!ip.IsLoopback() && !useTLS) {
		return nil, errors.New("private LAN requires TLS certificate and key")
	}
	var certificate tls.Certificate
	if useTLS {
		certificate, err = tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return nil, errors.New("cannot load TLS certificate/key")
		}
	}
	if cfg.PublicOrigin != "" {
		if _, err := validOrigin(cfg.PublicOrigin, true); err != nil {
			return nil, err
		}
	}
	listener, err := net.Listen("tcp", cfg.Address)
	if err != nil {
		return nil, err
	}
	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	origin := scheme + "://" + listener.Addr().String()
	origins := []string{origin}
	if cfg.PublicOrigin != "" {
		origins = append(origins, cfg.PublicOrigin)
	}
	token, path, err := createCredential(cfg.TokenFile)
	if err != nil {
		listener.Close()
		return nil, err
	}
	handler, err := NewHandler(hub, token, origins)
	if err != nil {
		listener.Close()
		os.Remove(path)
		return nil, err
	}
	server := &Server{URL: origin, TokenFile: path, hub: hub, done: make(chan struct{})}
	server.HTTP = &http.Server{Handler: handler, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	if useTLS {
		listener = tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}})
	}
	go func() {
		err := server.HTTP.Serve(listener)
		server.mu.Lock()
		if !errors.Is(err, http.ErrServerClosed) {
			server.serveErr = err
		}
		server.mu.Unlock()
		close(server.done)
	}()
	return server, nil
}

func (s *Server) Err() error { s.mu.Lock(); defer s.mu.Unlock(); return s.serveErr }
func (s *Server) Stop(ctx context.Context) error {
	s.hub.Close()
	err := s.HTTP.Shutdown(ctx)
	if err != nil {
		_ = s.HTTP.Close()
	}
	<-s.done
	removeErr := os.Remove(s.TokenFile)
	if err != nil {
		return err
	}
	if removeErr != nil && !os.IsNotExist(removeErr) {
		return removeErr
	}
	return s.Err()
}

func validOrigin(raw string, httpsOnly bool) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || (httpsOnly && parsed.Scheme != "https") {
		return nil, errors.New("origin must be an exact HTTPS origin without path or credentials")
	}
	return parsed, nil
}

func NewHandler(hub *Hub, token string, origins []string) (http.Handler, error) {
	if hub == nil || len(token) < 64 || len(origins) == 0 {
		return nil, errors.New("hub, strong token and origins required")
	}
	hosts := map[string]bool{}
	allowed := map[string]bool{}
	for _, origin := range origins {
		parsed, err := validOrigin(origin, false)
		if err != nil {
			return nil, err
		}
		// Hosts (RFC 3986) and origins (RFC 6454) compare case-insensitively;
		// browsers send them lower-cased whatever --public-origin said.
		hosts[strings.ToLower(parsed.Host)] = true
		allowed[strings.ToLower(origin)] = true
	}
	expected := sha256.Sum256([]byte("Bearer " + token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		w.Header().Set("Content-Type", "application/json")
		fail := func(code int, message string) {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
		}
		host := strings.ToLower(r.Host)
		if !hosts[host] || r.URL.RawQuery != "" {
			fail(403, "host or query refused")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			parsed, err := validOrigin(origin, false)
			if err != nil || !allowed[strings.ToLower(origin)] || !strings.EqualFold(parsed.Host, r.Host) {
				fail(403, "origin refused")
				return
			}
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			fail(403, "cross-site request refused")
			return
		}
		if len(r.Header.Values("Authorization")) != 1 {
			fail(401, "authentication required")
			return
		}
		actual := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(actual[:], expected[:]) != 1 {
			fail(401, "authentication required")
			return
		}
		switch {
		case r.URL.Path == "/v1/status" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(hub.Snapshot())
		case r.URL.Path == "/v1/actions" && r.Method == http.MethodPost:
			if r.Header.Get("Content-Type") != "application/json" {
				fail(415, "application/json required")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 16384)
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			var action Action
			if err := decoder.Decode(&action); err != nil {
				fail(400, "invalid or oversized JSON")
				return
			}
			if err := decoder.Decode(new(any)); err != io.EOF {
				fail(400, "trailing JSON refused")
				return
			}
			result, err := hub.Submit(action)
			if err != nil {
				code := 400
				if errors.Is(err, ErrReplay) || errors.Is(err, ErrDrift) {
					code = 409
				}
				if errors.Is(err, ErrUnavailable) {
					code = 503
				}
				fail(code, err.Error())
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(result)
		case strings.HasPrefix(r.URL.Path, "/v1/actions/") && r.Method == http.MethodGet:
			result, ok := hub.Result(strings.TrimPrefix(r.URL.Path, "/v1/actions/"))
			if !ok {
				fail(404, "request not found")
				return
			}
			_ = json.NewEncoder(w).Encode(result)
		default:
			fail(404, "endpoint not found")
		}
	}), nil
}

func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func createCredential(path string) (string, string, error) {
	var file *os.File
	var err error
	if path == "" {
		file, err = os.CreateTemp("", "cove-remote-*.token")
	} else {
		file, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	}
	if err != nil {
		return "", "", errors.New("cannot create new credential file")
	}
	path = file.Name()
	success := false
	defer func() {
		file.Close()
		if !success {
			os.Remove(path)
		}
	}()
	if err = privateCredential(file); err != nil {
		return "", "", errors.New("cannot secure credential file")
	}
	var random [32]byte
	if _, err = rand.Read(random[:]); err != nil {
		return "", "", err
	}
	token := hex.EncodeToString(random[:])
	if _, err = file.WriteString(token + "\n"); err != nil {
		return "", "", err
	}
	if err = file.Close(); err != nil {
		return "", "", err
	}
	success = true
	return token, path, nil
}
