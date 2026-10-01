package controllers

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"games-on-whales.github.io/direwolf/pkg/generic"
)

// libraryActivityFlushInterval is how often observed browser traffic is
// written to the pod. Far below the idle timeout, so the lag doesn't matter.
const libraryActivityFlushInterval = 30 * time.Second

// LibraryServer is the Library page's HTTP front end; the Ingress points
// here, not at the pod. With no pod it starts one (unless a game holds the
// Steam lock) and serves a "Starting…" page until the pod is ready, then
// reverse-proxies the Selkies page and its WebSocket to the pod.
type LibraryServer struct {
	library *LibraryController
	pods    generic.NamespacedLister[*corev1.Pod]
	// trusted are the only peers (the ingress controller behind Authentik)
	// allowed in: the desktop has no login of its own.
	trusted []netip.Prefix
	proxy   *httputil.ReverseProxy

	// lastSeen is the unix-nano time of the last byte to or from the pod.
	lastSeen    atomic.Int64
	lastFlushed int64
}

type libraryTargetKey struct{}

func NewLibraryServer(library *LibraryController, pods generic.NamespacedLister[*corev1.Pod], trusted []netip.Prefix) *LibraryServer {
	s := &LibraryServer{library: library, pods: pods, trusted: trusted}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		// Every byte through a pod connection, including an upgraded
		// WebSocket's, counts as browser activity.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, fmt.Errorf("dialing the Library pod: %w", err)
			}
			s.touch()
			return &activityConn{Conn: conn, touch: s.touch}, nil
		},
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	s.proxy = &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(r *httputil.ProxyRequest) {
			if target, ok := r.In.Context().Value(libraryTargetKey{}).(*url.URL); ok {
				r.SetURL(target)
			}
			r.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			klog.V(2).Infof("Library proxy error: %v", err)
			writeLibraryPage(w, http.StatusBadGateway, "The Library isn't answering yet. Retrying…", true)
		},
	}
	return s
}

func (s *LibraryServer) touch() {
	s.lastSeen.Store(time.Now().UnixNano())
}

// Run serves on port until ctx is done, flushing activity to the pod.
func (s *LibraryServer) Run(ctx context.Context, port int) error {
	server := &http.Server{
		Addr:              ":" + strconv.Itoa(port),
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	go func() {
		ticker := time.NewTicker(libraryActivityFlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.flushActivity(ctx)
			}
		}
	}()
	klog.Infof("Serving the Library page on :%d", port)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving the Library page: %w", err)
	}
	return nil
}

func (s *LibraryServer) flushActivity(ctx context.Context) {
	seen := s.lastSeen.Load()
	if seen == 0 || seen == s.lastFlushed {
		return
	}
	if err := s.library.RecordActivity(ctx, time.Unix(0, seen)); err != nil {
		klog.Errorf("Failed to record Library activity: %v", err)
		return
	}
	s.lastFlushed = seen
}

func (s *LibraryServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil || !s.isTrusted(peer.Addr().Unmap()) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	pod, err := s.pods.Get(LibraryPodName)
	if err != nil {
		pod = nil
	}
	if pod == nil {
		// Only a page visit starts the Library. A WebSocket reconnect or an
		// XHR from a tab left open after an idle shutdown must not.
		if r.Method != http.MethodGet || r.Header.Get("Upgrade") != "" {
			writeLibraryPage(w, http.StatusServiceUnavailable, "The Library is stopped. Reload the page to start it.", false)
			return
		}
		var busy string
		pod, busy, err = s.library.EnsurePod(r.Context())
		if err != nil {
			klog.Errorf("Failed to start the Library: %v", err)
			writeLibraryPage(w, http.StatusInternalServerError, "The Library could not be started. Try again in a minute.", false)
			return
		}
		if busy != "" {
			writeLibraryPage(w, http.StatusConflict, busy, false)
			return
		}
	}

	switch {
	case pod.DeletionTimestamp != nil:
		writeLibraryPage(w, http.StatusServiceUnavailable, "The Library is shutting down. It will start again in a moment…", true)
	case !podReady(pod) || pod.Status.PodIP == "":
		writeLibraryPage(w, http.StatusServiceUnavailable, "Starting the Library…", true)
	default:
		s.touch()
		target := &url.URL{Scheme: "http", Host: net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(libraryHTTPPort))}
		s.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), libraryTargetKey{}, target)))
	}
}

func (s *LibraryServer) isTrusted(addr netip.Addr) bool {
	for _, prefix := range s.trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// writeLibraryPage serves a minimal status page; refresh reloads it until the
// Library is up.
func writeLibraryPage(w http.ResponseWriter, status int, message string, refresh bool) {
	meta := ""
	if refresh {
		meta = `<meta http-equiv="refresh" content="3">`
		w.Header().Set("Retry-After", "3")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">%s<title>Library</title>
<style>body{font-family:system-ui,sans-serif;background:#111;color:#eee;display:grid;place-items:center;min-height:100vh;margin:0;padding:0 16px}p{max-width:32em;text-align:center;font-size:1.2em}</style>
</head><body><p>%s</p></body></html>
`, meta, html.EscapeString(message))
}

// activityConn calls touch on every read or write.
type activityConn struct {
	net.Conn
	touch func()
}

func (c *activityConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.touch()
	}
	return n, err //nolint:wrapcheck // net.Conn passthrough; io.EOF must stay io.EOF
}

func (c *activityConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.touch()
	}
	return n, err //nolint:wrapcheck // net.Conn passthrough
}
