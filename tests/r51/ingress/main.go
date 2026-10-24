// Command ingress is a test-only TLS terminator for R5.1 acceptance.
//
// It listens on loopback only, terminates TLS with a test CA certificate, strips
// every client-supplied forwarding header, injects trusted X-Forwarded-* values,
// and proxies to the loopback onboardd instance. The plain-HTTP listener only
// redirects to HTTPS (308). It is NOT a production ingress.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type options struct {
	listen     string
	httpListen string
	upstream   string
	certFile   string
	keyFile    string
}

func main() {
	var opt options
	flag.StringVar(&opt.listen, "listen", "127.0.0.1:18443", "HTTPS listen address (loopback only)")
	flag.StringVar(&opt.httpListen, "http-listen", "127.0.0.1:18080", "plain-HTTP redirect listen address (loopback only)")
	flag.StringVar(&opt.upstream, "upstream", "http://127.0.0.1:18788", "loopback upstream base URL")
	flag.StringVar(&opt.certFile, "cert", "", "server certificate PEM")
	flag.StringVar(&opt.keyFile, "key", "", "server private key PEM")
	flag.Parse()
	if err := run(opt); err != nil {
		fmt.Fprintln(os.Stderr, "ingress:", err)
		os.Exit(1)
	}
}

func run(opt options) error {
	if err := requireLoopback(opt.listen); err != nil {
		return fmt.Errorf("https listener: %w", err)
	}
	if err := requireLoopback(opt.httpListen); err != nil {
		return fmt.Errorf("http listener: %w", err)
	}
	upstream, err := url.Parse(opt.upstream)
	if err != nil || upstream.Scheme != "http" || upstream.Host == "" {
		return fmt.Errorf("upstream must be an absolute http:// URL")
	}
	if err := requireLoopback(upstream.Host); err != nil {
		return fmt.Errorf("upstream: %w", err)
	}
	if opt.certFile == "" || opt.keyFile == "" {
		return fmt.Errorf("cert and key are required")
	}
	cert, err := tls.LoadX509KeyPair(opt.certFile, opt.keyFile)
	if err != nil {
		return fmt.Errorf("load certificate: %w", err)
	}

	httpsSrv := &http.Server{
		Addr:              opt.listen,
		Handler:           newProxyHandler(upstream),
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		},
	}
	httpSrv := &http.Server{
		Addr:              opt.httpListen,
		Handler:           newRedirectHandler(portOf(opt.listen)),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errc := make(chan error, 2)
	go func() {
		log.Printf(`{"msg":"https listening","addr":%q}`, opt.listen)
		if err := httpsSrv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			errc <- err
		}
	}()
	go func() {
		log.Printf(`{"msg":"http redirect listening","addr":%q}`, opt.httpListen)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errc <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errc:
		return err
	case <-stop:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpsSrv.Shutdown(ctx)
	_ = httpSrv.Shutdown(ctx)
	return nil
}

// newProxyHandler returns a handler that overwrites all forwarding headers.
func newProxyHandler(upstream *url.URL) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			// Keep the original Host so the application's Origin/Referer checks
			// compare against the browser-visible host.
			pr.Out.Host = pr.In.Host
			for _, h := range []string{
				"Forwarded", "X-Forwarded-For", "X-Forwarded-Host",
				"X-Forwarded-Proto", "X-Forwarded-Port", "X-Real-Ip",
			} {
				pr.Out.Header.Del(h)
			}
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
			pr.Out.Header.Set("X-Forwarded-Port", "443")
			if ip, _, err := net.SplitHostPort(pr.In.RemoteAddr); err == nil {
				pr.Out.Header.Set("X-Forwarded-For", ip)
				pr.Out.Header.Set("X-Real-Ip", ip)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf(`{"msg":"upstream error","path":%q}`, r.URL.Path)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
		FlushInterval: 100 * time.Millisecond,
	}
	return proxy
}

// newRedirectHandler redirects plain HTTP to HTTPS on tlsPort.
func newRedirectHandler(tlsPort string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		target := "https://" + net.JoinHostPort(host, tlsPort) + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
	})
}

func portOf(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "443"
	}
	return port
}

// requireLoopback rejects wildcard or non-loopback listen/upstream addresses.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("address %q must be host:port", addr)
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("address %q has an empty host", addr)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("host %q is not loopback; the test ingress only binds 127.0.0.1/::1", host)
	}
	return nil
}
