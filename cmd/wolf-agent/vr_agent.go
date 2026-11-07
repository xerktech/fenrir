package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/godbus/dbus/v5"
	"k8s.io/klog/v2"

	"games-on-whales.github.io/direwolf/pkg/controllers"
	"games-on-whales.github.io/direwolf/pkg/util"
	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

// vrAgentCommand runs the vr-agent sidecar of a native VR Session's pod
// (XERK-2668): it reads WiVRn's state from the App's session bus and serves it
// to the operator, behind the Session's bearer token and pinned cert. The pod
// is off the host network, so only the pod network reaches it.
const vrAgentCommand = "vr-agent"

// WiVRn's D-Bus server object (WiVRn dbus/io.github.wivrn.Server.xml).
const (
	wivrnService   = "io.github.wivrn.Server"
	wivrnPath      = dbus.ObjectPath("/io/github/wivrn/Server")
	wivrnInterface = "io.github.wivrn.Server"
	// Each read dials the bus afresh: the bus and WiVRn come and go with
	// the App's VR services (vr-services restarts wivrn-server).
	vrBusTimeout = 5 * time.Second
	// maxReportedKeys bounds KnownKeys in a reply: well over the operator's
	// per-User cap, well under its 64KiB read (a key and name are < 1.2KiB).
	maxReportedKeys = 32
)

// wivrnStatus reads WiVRn's HeadsetConnected and SessionRunning from the bus
// at address.
func wivrnStatus(ctx context.Context, address string) (controllers.VRStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, vrBusTimeout)
	defer cancel()
	conn, err := dbus.Connect(address, dbus.WithContext(ctx))
	if err != nil {
		return controllers.VRStatus{}, fmt.Errorf("connecting to the session bus: %w", err)
	}
	defer conn.Close()
	var props map[string]dbus.Variant
	err = conn.Object(wivrnService, wivrnPath).
		CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", 0, wivrnInterface).
		Store(&props)
	if err != nil {
		return controllers.VRStatus{}, fmt.Errorf("reading WiVRn's properties: %w", err)
	}
	var st controllers.VRStatus
	for name, dst := range map[string]*bool{"HeadsetConnected": &st.HeadsetConnected, "SessionRunning": &st.SessionRunning} {
		v, ok := props[name]
		if !ok {
			return controllers.VRStatus{}, fmt.Errorf("WiVRn has no %s property", name)
		}
		if err := v.Store(dst); err != nil {
			return controllers.VRStatus{}, fmt.Errorf("WiVRn's %s: %w", name, err)
		}
	}
	// The keys WiVRn admits, a(ssx): name, key, last connection. The
	// operator records a newly paired one as a Headset (XERK-2669).
	if v, ok := props["KnownKeys"]; ok {
		var keys []struct {
			Name, Key      string
			LastConnection int64
		}
		if err := v.Store(&keys); err != nil {
			return controllers.VRStatus{}, fmt.Errorf("WiVRn's KnownKeys: %w", err)
		}
		// Bounded, so a game flooding the keys file can't grow the reply
		// past what the operator reads (it would read as a lost headset).
		for _, k := range keys[:min(len(keys), maxReportedKeys)] {
			st.KnownKeys = append(st.KnownKeys, controllers.VRKnownKey{Name: k.Name, Key: k.Key})
		}
	}
	return st, nil
}

// vrAgentHandler serves /readyz (WiVRn answers), /livez, and the status
// behind the bearer token.
func vrAgentHandler(token func() string, status func(context.Context) (controllers.VRStatus, error)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if _, err := status(r.Context()); err != nil {
			klog.V(2).InfoS("Not ready", "err", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle(controllers.VRStatusPath, wolfapi.RequireBearerToken(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		st, err := status(r.Context())
		if err != nil {
			// The bus's error stays in the log: it names paths in the pod.
			klog.ErrorS(err, "Reading WiVRn's state")
			http.Error(w, "VR server unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	})))
	return mux
}

func runVRAgent(args []string) {
	fs := flag.NewFlagSet(vrAgentCommand, flag.ExitOnError)
	port := fs.Int("port", int(controllers.VRAgentPort), "Port to serve HTTPS on")
	bus := fs.String("bus", controllers.VRBusAddress, "D-Bus address of the App's session bus")
	tokenPath := fs.String("token-file", "", "Path to a file holding the bearer token (required)")
	certPath := fs.String("tls-cert", "", "Path to the serving cert (required)")
	keyPath := fs.String("tls-key", "", "Path to the serving key (required)")
	klog.InitFlags(fs)
	if err := fs.Parse(args); err != nil {
		klog.Fatal(err)
	}
	token, err := newTokenFile(*tokenPath)
	if err != nil {
		klog.Fatal("Failed to load bearer token: ", err)
	}
	cert, err := util.LoadCertificates(*certPath, *keyPath)
	if err != nil {
		klog.Fatal("Failed to load certificates: ", err)
	}
	certs := newCertFile(*certPath, *keyPath, &cert)
	status := func(ctx context.Context) (controllers.VRStatus, error) { return wivrnStatus(ctx, *bus) }
	server := newServer(*port, vrAgentHandler(token.Token, status), serverTLSConfig(certs))
	klog.Infof("vr-agent listening on port %d, reading WiVRn on %s", *port, *bus)
	if err := server.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		klog.Error("vr-agent: ", err)
		os.Exit(1)
	}
}
