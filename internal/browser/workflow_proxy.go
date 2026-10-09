//go:build chromedp

package browser

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

type workflowProxy struct {
	address   string
	blocked   atomic.Bool
	failed    atomic.Bool
	server    *http.Server
	transport *http.Transport
}

func (b *Browser) workflowDial(ctx context.Context, networkName, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("invalid network destination")
	}
	addresses, err := b.workflowAddresses(ctx, host)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	// Every address passed the same checks; a host whose AAAA record comes
	// first on a machine without IPv6 must not make the whole run
	// network_unavailable.
	var lastErr error
	for _, address := range addresses {
		conn, err := dialer.DialContext(ctx, networkName, net.JoinHostPort(address.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

func (b *Browser) startWorkflowProxy(ctx context.Context) (*workflowProxy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	proxy := &workflowProxy{address: listener.Addr().String()}
	proxy.transport = &http.Transport{Proxy: nil, DialContext: b.workflowDial, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second, MaxIdleConns: 4}
	proxy.server = &http.Server{ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 32 * 1024}
	proxy.server.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodConnect {
			connection, err := b.workflowDial(ctx, "tcp", request.Host)
			if err != nil {
				if errors.Is(err, errWorkflowSafety) {
					proxy.blocked.Store(true)
				} else {
					proxy.failed.Store(true)
				}
				http.Error(writer, "destination blocked", http.StatusForbidden)
				return
			}
			client, buffered, err := writer.(http.Hijacker).Hijack()
			if err != nil {
				_ = connection.Close()
				return
			}
			if deadline, ok := ctx.Deadline(); ok {
				_ = connection.SetDeadline(deadline)
				_ = client.SetDeadline(deadline)
			}
			_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
			_ = buffered.Flush()
			closed := make(chan struct{})
			go func() {
				select {
				case <-ctx.Done():
					_ = client.Close()
					_ = connection.Close()
				case <-closed:
				}
			}()
			go func() { _, _ = io.Copy(connection, buffered); _ = connection.Close() }()
			_, _ = io.Copy(client, connection)
			_ = client.Close()
			_ = connection.Close()
			close(closed)
			return
		}
		if request.URL == nil || b.workflowURL(ctx, request.URL.String()) != nil || request.Header.Get("Upgrade") != "" {
			proxy.blocked.Store(true)
			http.Error(writer, "destination blocked", http.StatusForbidden)
			return
		}
		outbound := request.Clone(ctx)
		outbound.RequestURI = ""
		outbound.Header.Del("Proxy-Authorization")
		outbound.Header.Del("Proxy-Connection")
		outbound.Header.Del("Connection")
		response, err := proxy.transport.RoundTrip(outbound)
		if err != nil {
			if errors.Is(err, errWorkflowSafety) {
				proxy.blocked.Store(true)
			} else {
				proxy.failed.Store(true)
			}
			http.Error(writer, "request unavailable", http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for key, values := range response.Header {
			for _, value := range values {
				writer.Header().Add(key, value)
			}
		}
		writer.WriteHeader(response.StatusCode)
		_, _ = io.Copy(writer, response.Body)
	})
	go func() { _ = proxy.server.Serve(listener) }()
	return proxy, nil
}

func (proxy *workflowProxy) close() {
	_ = proxy.server.Close()
	proxy.transport.CloseIdleConnections()
}
