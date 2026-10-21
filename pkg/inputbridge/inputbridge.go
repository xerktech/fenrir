// Package inputbridge publishes hotplugged /dev/input nodes into a
// user-namespaced App pod (XERK-1920).
//
// The kernel refuses device mknod inside a user namespace, and the runtime
// mounts every volume of a hostUsers: false pod nodev, so neither the App nor
// a node created in a shared volume works there. A device node is openable in
// the App pod only as a mount of its own from the host's mount namespace. The
// bridge runs privileged in the session's Wolf pod with the sessions host
// directory mounted Bidirectional: it mknods each node into the session's
// input directory and bind-mounts it onto itself. The mount propagates to the
// host and on into the App pod, which mounts that directory read-only with
// HostToContainer propagation.
//
// wolf-agent asks for each node over a unix socket on a volume only the two of
// them mount (Client). The bridge creates only major 13 nodes directly in its
// directory, as fakeudev.CreateDeviceNode does for wolf-agent's own volume.
package inputbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"k8s.io/klog/v2"

	"games-on-whales.github.io/direwolf/pkg/fakeudev"
)

// Request ops.
const (
	OpCreate = "create"
	OpRemove = "remove"
	OpClear  = "clear"
)

// Request is one line a Client sends: an op and, for create and remove, the
// udev properties of the device (DEVNAME, MAJOR, MINOR).
type Request struct {
	Op    string            `json:"op"`
	Props map[string]string `json:"props,omitempty"`
}

// Response is the bridge's one-line answer; Error is empty on success.
type Response struct {
	Error string `json:"error,omitempty"`
}

// inputMajor is /dev/input's char major, the only one the bridge publishes.
const inputMajor = "13"

// maxRequestBytes bounds one request line. A device's properties are a few
// hundred bytes.
const maxRequestBytes = 64 << 10

// Server publishes device nodes in InputDir, the session's input directory.
type Server struct {
	InputDir string

	// Bind-mount a node onto itself, and undo it. Variables so tests run
	// without CAP_SYS_ADMIN.
	bindSelf func(path string) error
	unmount  func(path string) error
	// Creates the node; fakeudev.CreateDeviceNode outside tests.
	createNode func(dir string, props map[string]string) error
	// Checks path is a char device before it is bind-mounted.
	checkNode func(path string) error
}

// NewServer returns a Server publishing into inputDir.
func NewServer(inputDir string) *Server {
	return &Server{
		InputDir:   inputDir,
		bindSelf:   bindSelf,
		unmount:    unmount,
		createNode: fakeudev.CreateDeviceNode,
		checkNode:  isCharDevice,
	}
}

// LockedOwner owns the directories the App pod mounts read-only (LockDir).
// The App pod's idmapped mounts map only the pod's own 65536 IDs, so to the
// App this owner is unmapped: even its root, after remounting the mount
// read-write (which a user namespace allows, measured on talos04), may not
// create, rename or remove entries there. Root on the node still may.
const LockedOwner = 4294967294

// LockDir makes dir owned by LockedOwner and 0755, so only the node's root
// (the bridge, wolf-agent) changes what is in it. Run it before trusting
// what dir holds, then clear what the App may have put there first.
func LockDir(dir string) error {
	if err := os.Lchown(dir, LockedOwner, LockedOwner); err != nil {
		return fmt.Errorf("chown %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return fmt.Errorf("chmod %s: %w", dir, err)
	}
	return nil
}

func isCharDevice(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat new node: %w", err)
	}
	if fi.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("%s is %v, not a char device", path, fi.Mode())
	}
	return nil
}

// Serve answers requests on l until ctx ends, one connection at a time:
// wolf-agent handles Wolf's events in order, and serial requests keep a
// create and a clear from interleaving.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	for {
		conn, err := l.Accept()
		switch {
		case err != nil && ctx.Err() != nil:
			return nil //nolint:nilerr // the listener was closed for ctx
		case err != nil:
			return fmt.Errorf("accept: %w", err)
		}
		s.serveConn(conn)
	}
}

func (s *Server) serveConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReaderSize(conn, maxRequestBytes).ReadSlice('\n')
	if err != nil {
		klog.Warningf("input-bridge: reading request: %v", err)
		return
	}
	var req Request
	resp := Response{}
	if err = json.Unmarshal(line, &req); err != nil {
		resp.Error = fmt.Sprintf("bad request: %v", err)
	} else if err = s.Handle(req); err != nil {
		resp.Error = err.Error()
	}
	if resp.Error != "" {
		klog.Warningf("input-bridge: %s %q: %s", req.Op, req.Props["DEVNAME"], resp.Error)
	} else {
		klog.InfoS("input-bridge", "op", req.Op, "devname", req.Props["DEVNAME"])
	}
	out, err := json.Marshal(resp)
	if err != nil {
		klog.Errorf("input-bridge: encoding response: %v", err)
		return
	}
	_, _ = conn.Write(append(out, '\n'))
}

// Handle runs one request.
func (s *Server) Handle(req Request) error {
	switch req.Op {
	case OpCreate:
		return s.create(req.Props)
	case OpRemove:
		return s.remove(req.Props)
	case OpClear:
		return s.Clear()
	}
	return fmt.Errorf("unknown op %q", req.Op)
}

func (s *Server) create(props map[string]string) error {
	path, err := s.nodePath(props)
	if err != nil {
		return err
	}
	// A stale node of the same name may still be mounted: removing it
	// would fail with EBUSY.
	if err := s.unmount(path); err != nil {
		return err
	}
	if err := s.createNode(s.InputDir, props); err != nil {
		return err
	}
	if err := s.checkNode(path); err != nil {
		return err
	}
	if err := s.bindSelf(path); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("bind-mount %s: %w", path, err)
	}
	return nil
}

func (s *Server) remove(props map[string]string) error {
	path, err := s.nodePath(props)
	if err != nil {
		return err
	}
	if err := s.unmount(path); err != nil {
		return err
	}
	if err := fakeudev.RemoveDeviceNode(s.InputDir, props); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// Clear unmounts and removes every node in InputDir. Wolf destroys a
// stopped stream's devices without unplug events (see Agent.clearDevices).
func (s *Server) Clear() error {
	entries, err := os.ReadDir(s.InputDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", s.InputDir, err)
	}
	var errs []error
	for _, e := range entries {
		path := filepath.Join(s.InputDir, e.Name())
		if err := s.unmount(path); err != nil {
			errs = append(errs, err)
			continue
		}
		// RemoveAll: an App could have left a directory here before the
		// bridge locked InputDir. It doesn't follow symlinks.
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// nodePath is where props' node lives, refusing what fakeudev would before
// anything in the directory is touched.
func (s *Server) nodePath(props map[string]string) (string, error) {
	if !fakeudev.IsInputDeviceNode(props) {
		return "", fmt.Errorf("not an input device node: %q", props["DEVNAME"])
	}
	if props["MAJOR"] != inputMajor {
		return "", fmt.Errorf("refusing %q with major %q (only %s)", props["DEVNAME"], props["MAJOR"], inputMajor)
	}
	return filepath.Join(s.InputDir, filepath.Base(props["DEVNAME"])), nil
}

// Client sends requests to a Server's socket.
type Client struct {
	Socket string
	// Bounds one request, dial included.
	Timeout time.Duration
}

// Create publishes props' device node.
func (c *Client) Create(props map[string]string) error {
	return c.do(Request{Op: OpCreate, Props: props})
}

// Remove withdraws props' device node.
func (c *Client) Remove(props map[string]string) error {
	return c.do(Request{Op: OpRemove, Props: props})
}

// Clear withdraws every device node.
func (c *Client) Clear() error {
	return c.do(Request{Op: OpClear})
}

func (c *Client) do(req Request) error {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return fmt.Errorf("input-bridge: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	out, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("input-bridge: encoding request: %w", err)
	}
	if _, err = conn.Write(append(out, '\n')); err != nil {
		return fmt.Errorf("input-bridge: %w", err)
	}
	line, err := bufio.NewReaderSize(conn, maxRequestBytes).ReadSlice('\n')
	if err != nil {
		return fmt.Errorf("input-bridge: reading response: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("input-bridge: bad response: %w", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("input-bridge: %s", resp.Error)
	}
	return nil
}
