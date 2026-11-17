// Package appcontrol serves the per-run listening-socket control channel.
//
// The supervisor creates one inherited AF_UNIX SOCK_SEQPACKET socketpair per
// supervised run, passes the child end to the app as fd 3 and records its
// number in CAPSULAD_CONTROL_FD. The app sends one JSON request per message;
// the daemon replies with one JSON response and, for a successful listener,
// one file descriptor attached as SCM_RIGHTS in the same message.
//
// The daemon allocates the bound socket and transfers it. Apps never create or
// bind the listening socket themselves, so bind policy (reserved ports,
// multicast rejection, listener count) is enforced in one place.
package appcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"

	"capsulad/internal/logbus"
)

const (
	// EnvName is the app environment variable holding the control fd number.
	EnvName = "CAPSULAD_CONTROL_FD"

	// MaxListenersPerRun bounds concurrent listener allocations.
	MaxListenersPerRun = 16

	// maxRequestBytes bounds one control request. Requests are small JSON
	// objects; the bound also keeps the reply/request pair inside a host
	// Unix-datagram limit used by non-Linux test sockets.
	maxRequestBytes = 1024
	// maxResponseBytes bounds one control response.
	maxResponseBytes = 1024
	defaultBacklog   = 64

	// Response status values used by the native SDK to classify a refusal.
	// They are stable numbers, not gRPC codes: the SDK maps them onto its own
	// status enum.
	StatusInvalidArgument int32 = 1
	StatusTransport       int32 = 7
)

// Policy is the listening-socket policy for supervised apps.
type Policy struct {
	// ReservedPorts are refused in addition to the mDNS port. The daemon
	// includes the configured Web UI port.
	ReservedPorts []int
}

type Manager struct {
	logs   *logbus.Bus
	policy Policy

	mu     sync.Mutex
	runs   map[string]*runChannel
	closed bool
}

// New returns a manager that refuses ports in policy.ReservedPorts and the
// mDNS port 5353.
func New(logs *logbus.Bus, policy Policy) *Manager {
	return &Manager{logs: logs, policy: policy, runs: map[string]*runChannel{}}
}

// Channel creates the per-run socketpair. The caller passes the returned child
// end via cmd.ExtraFiles and puts 3 in CAPSULAD_CONTROL_FD. abort must be
// called when the process never starts; CloseRun releases it otherwise.
func (m *Manager) Channel(runID, appID string) (child *os.File, abort func(), err error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, nil, errors.New("control channel manager is closed")
	}
	if _, exists := m.runs[runID]; exists {
		m.mu.Unlock()
		return nil, nil, fmt.Errorf("control channel for run %s already exists", runID)
	}
	m.mu.Unlock()

	fds, err := controlSocketPair()
	if err != nil {
		return nil, nil, fmt.Errorf("control socketpair: %w", err)
	}
	parentFD, childFD := fds[0], fds[1]
	// ExtraFiles inherits the child; the parent stays daemon-only. The child
	// is close-on-exec in the daemon so a failed spawn cannot leak it.
	unix.CloseOnExec(parentFD)
	unix.CloseOnExec(childFD)
	// The parent participates in the Go poller so CloseRun unblocks a pending
	// read instead of waiting for the app to send another request.
	if err := unix.SetNonblock(parentFD, true); err != nil {
		_ = unix.Close(parentFD)
		_ = unix.Close(childFD)
		return nil, nil, fmt.Errorf("control socket: %w", err)
	}
	parent := os.NewFile(uintptr(parentFD), "capsulad-control-"+runID)
	child = os.NewFile(uintptr(childFD), "capsulad-control-child-"+runID)
	if parent == nil || child == nil {
		if parent != nil {
			_ = parent.Close()
		}
		if child != nil {
			_ = child.Close()
		}
		return nil, nil, errors.New("control socket: cannot wrap descriptors")
	}
	run := &runChannel{manager: m, runID: runID, appID: appID, file: parent}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = parent.Close()
		_ = child.Close()
		return nil, nil, errors.New("control channel manager is closed")
	}
	m.runs[runID] = run
	m.mu.Unlock()

	go run.serve()
	var once sync.Once
	return child, func() { once.Do(func() { m.CloseRun(runID) }) }, nil
}

// CloseRun closes the control channel and drops the run's accounting. It is
// idempotent and is called on run exit and failed spawn.
func (m *Manager) CloseRun(runID string) {
	m.mu.Lock()
	run := m.runs[runID]
	delete(m.runs, runID)
	m.mu.Unlock()
	if run != nil {
		run.close()
	}
}

// Close closes every run channel and refuses new ones.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	runs := make([]*runChannel, 0, len(m.runs))
	for _, run := range m.runs {
		runs = append(runs, run)
	}
	m.runs = nil
	m.mu.Unlock()
	for _, run := range runs {
		run.close()
	}
}

type runChannel struct {
	manager *Manager
	runID   string
	appID   string
	file    *os.File

	mu          sync.Mutex
	listeners   int
	allocations map[uint64]*listenerAlloc
	nextAlloc   uint64
	closed      bool
	once        sync.Once
}

func (r *runChannel) close() {
	r.once.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		_ = r.file.Close()
		r.mu.Lock()
		allocations := r.allocations
		r.allocations = nil
		r.listeners = 0
		r.mu.Unlock()
		for _, alloc := range allocations {
			_ = alloc.file.Close()
		}
	})
}

func (r *runChannel) serve() {
	buf := make([]byte, maxRequestBytes+1)
	for {
		n, err := r.file.Read(buf)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				r.manager.logf(r.appID, "control channel run=%s closed: %v", r.runID, err)
			}
			return
		}
		if n == 0 {
			return
		}
		if n > maxRequestBytes {
			r.replyError(0, "request exceeds the control message size limit", StatusInvalidArgument)
			return
		}
		response, listener := r.handle(buf[:n])
		fd := -1
		if listener != nil {
			fd = int(listener.file.Fd())
		}
		if err := r.send(marshalResponse(response), fd); err != nil {
			if listener != nil {
				_ = listener.file.Close()
			}
			r.manager.logf(r.appID, "control reply run=%s: %v", r.runID, err)
			return
		}
		if listener != nil {
			// Keep the daemon's descriptor until the app releases the
			// allocation: the close request is the release signal, and the
			// retained file lets run cleanup reclaim it.
			if !r.retainAllocation(listener) {
				continue
			}
			r.manager.logf(r.appID, "listener allocated run=%s proto=%s address=%s port=%d",
				r.runID, listener.protocol, net.JoinHostPort(listener.address, strconv.Itoa(listener.port)), listener.port)
		}
	}
}

func (r *runChannel) replyError(id uint64, message string, code int32) {
	_ = r.send(marshalResponse(listenResponse{ID: id, Error: message, Status: code}), -1)
}

// send writes one response message; fd, when non-negative, travels as
// SCM_RIGHTS in the same message. The caller retains ownership of fd.
func (r *runChannel) send(payload []byte, fd int) error {
	if len(payload) > maxResponseBytes {
		return fmt.Errorf("response exceeds %d bytes", maxResponseBytes)
	}
	var oob []byte
	if fd >= 0 {
		oob = unix.UnixRights(fd)
	}
	rc, err := r.file.SyscallConn()
	if err != nil {
		return err
	}
	var sendErr error
	err = rc.Write(func(rawFD uintptr) bool {
		sendErr = unix.Sendmsg(int(rawFD), payload, oob, nil, 0)
		switch sendErr {
		case nil:
			return true
		case unix.EINTR:
			return false
		case unix.EAGAIN:
			return false
		default:
			return true
		}
	})
	if err != nil {
		return err
	}
	return sendErr
}

type listenRequest struct {
	// ID is a monotonic request counter chosen by the app. The reply echoes
	// it so the app can correlate a response with its request even while
	// another request is in flight on the shared channel.
	ID       uint64 `json:"id"`
	Op       string `json:"op"`
	Protocol string `json:"protocol"`
	Bind     string `json:"bind"`
	Port     int    `json:"port"`
	Backlog  int    `json:"backlog"`
	// JoinGroup requests a multicast receive socket for that IPv4 group. It is
	// accepted only for UDP on the PTP ports; see validate.
	JoinGroup string `json:"join_group,omitempty"`
	// Allocation names the listener a close request releases.
	Allocation uint64 `json:"allocation,omitempty"`
}

type listenResponse struct {
	// ID echoes listenRequest.ID; it is present on every reply, including
	// refusals. An undecodable request cannot name an id, so it replies with 0.
	ID       uint64 `json:"id"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Status   int32  `json:"status,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Address  string `json:"address,omitempty"`
	Port     int    `json:"port,omitempty"`
	Group    string `json:"group,omitempty"`
	// Allocation identifies the listener for a later close request, which is
	// how a run's live-listener accounting is released. It is echoed only on a
	// successful allocation.
	Allocation uint64 `json:"allocation,omitempty"`
}

func marshalResponse(resp listenResponse) []byte {
	data, _ := json.Marshal(resp) // fixed struct; marshal cannot fail
	return data
}

type listenerAlloc struct {
	id       uint64
	file     *os.File
	protocol string
	address  string
	port     int
	group    string
}

// handle decodes one request and returns the reply to send. Every reply
// echoes the request id, refusals included, so an app that has more than one
// request outstanding on the shared channel cannot mistake a reply for another
// request's. alloc is non-nil only for a successful listener allocation.
func (r *runChannel) handle(payload []byte) (listenResponse, *listenerAlloc) {
	var req listenRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errorResponse(0, StatusInvalidArgument, fmt.Errorf("decode control request: %w", err)), nil
	}
	resp := listenResponse{ID: req.ID}
	if req.Op == "close" {
		r.releaseAllocation(req.Allocation)
		// Closing is idempotent: an unknown or already-released allocation is
		// not an error, so an app can always clean up on every exit path.
		resp.OK = true
		return resp, nil
	}
	if req.Op != "listen" {
		return errorResponse(req.ID, StatusInvalidArgument,
			fmt.Errorf("unsupported control operation %q", req.Op)), nil
	}
	proto, ip, port, backlog, group, err := r.validate(req)
	if err != nil {
		return errorResponse(req.ID, StatusInvalidArgument, err), nil
	}
	alloc, err := r.listen(proto, ip, port, backlog, group)
	if err != nil {
		return errorResponse(req.ID, StatusTransport, err), nil
	}
	resp.OK = true
	resp.Protocol, resp.Address, resp.Port = alloc.protocol, alloc.address, alloc.port
	resp.Group = alloc.group
	resp.Allocation = r.nextAllocationID()
	alloc.id = resp.Allocation
	return resp, alloc
}

// nextAllocationID returns the candidate id for a new allocation. The id is
// included in the reply before the descriptor is sent; retainAllocation commits
// it only after the send succeeded.
func (r *runChannel) nextAllocationID() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextAlloc++
	return r.nextAlloc
}

// retainAllocation commits a listener after its descriptor was transferred. It
// enforces the live-listener cap at commit time (the bind pre-check can race)
// and is the point where a failed accounting leaves no listener behind.
func (r *runChannel) retainAllocation(alloc *listenerAlloc) bool {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = alloc.file.Close()
		return false
	}
	if r.listeners >= MaxListenersPerRun {
		r.mu.Unlock()
		_ = alloc.file.Close()
		r.manager.logf(r.appID, "listener refused run=%s: run already holds %d live listeners",
			r.runID, MaxListenersPerRun)
		return false
	}
	if r.allocations == nil {
		r.allocations = map[uint64]*listenerAlloc{}
	}
	r.allocations[alloc.id] = alloc
	r.listeners++
	r.mu.Unlock()
	return true
}

// releaseAllocation frees a live listener and the daemon's retained descriptor.
// It is idempotent, so a close that races run cleanup is harmless.
func (r *runChannel) releaseAllocation(id uint64) {
	if id == 0 {
		return
	}
	r.mu.Lock()
	alloc := r.allocations[id]
	if alloc != nil {
		delete(r.allocations, id)
		if r.listeners > 0 {
			r.listeners--
		}
	}
	r.mu.Unlock()
	if alloc != nil {
		_ = alloc.file.Close()
		r.manager.logf(r.appID, "listener released run=%s allocation=%d", r.runID, id)
	}
}

// errorResponse is one refusal reply. The id is carried separately from the
// request value so a refusal can be built before any field is decoded.
func errorResponse(id uint64, status int32, err error) listenResponse {
	return listenResponse{ID: id, Error: err.Error(), Status: status}
}

func (r *runChannel) validate(req listenRequest) (string, net.IP, int, int, net.IP, error) {
	protocol := strings.ToLower(strings.TrimSpace(req.Protocol))
	if protocol != "tcp" && protocol != "udp" {
		return "", nil, 0, 0, nil, fmt.Errorf("protocol must be tcp or udp")
	}
	ip, err := validateBind(req.Bind)
	if err != nil {
		return "", nil, 0, 0, nil, err
	}
	port := req.Port
	var group net.IP
	if strings.TrimSpace(req.JoinGroup) != "" {
		// A group receiver is a receive-only UDP socket on a PTP port: the
		// AirPlay timing stream arrives on the PTP event (319) and general
		// (320) ports addressed to 224.0.0.1. Anything else is refused so this
		// option cannot be used to open an arbitrary multicast door.
		if protocol != "udp" {
			return "", nil, 0, 0, nil, errors.New("join_group requires protocol \"udp\"")
		}
		group = net.ParseIP(strings.TrimSpace(req.JoinGroup))
		if group == nil || group.To4() == nil || !group.IsMulticast() {
			return "", nil, 0, 0, nil, fmt.Errorf("join_group %q is not an IPv4 multicast group", req.JoinGroup)
		}
		if !group.Equal(net.IPv4(224, 0, 0, 1)) {
			return "", nil, 0, 0, nil, fmt.Errorf("join_group %q is not the AirPlay PTP group 224.0.0.1", req.JoinGroup)
		}
		if port != 319 && port != 320 {
			return "", nil, 0, 0, nil, fmt.Errorf("join_group is only allowed on the PTP ports 319 and 320, not %d", port)
		}
		if !ip.IsUnspecified() {
			return "", nil, 0, 0, nil, fmt.Errorf("join_group needs the wildcard bind, not %q", ip)
		}
		return protocol, ip, port, 0, group, nil
	}
	if port == 0 {
		// ephemeral
	} else if port < 1024 || port > 65535 {
		return "", nil, 0, 0, nil, fmt.Errorf("port %d must be 0 or 1024..65535", port)
	}
	if port != 0 {
		if port == 5353 {
			return "", nil, 0, 0, nil, errors.New("port 5353 is reserved for mDNS")
		}
		for _, reserved := range r.manager.policy.ReservedPorts {
			if port == reserved {
				return "", nil, 0, 0, nil, fmt.Errorf("port %d is reserved by the daemon", port)
			}
		}
	}
	backlog := 0
	if protocol == "tcp" {
		backlog = req.Backlog
		if backlog == 0 {
			backlog = defaultBacklog
		}
		if backlog < 1 || backlog > 128 {
			return "", nil, 0, 0, nil, fmt.Errorf("backlog must be 1..128")
		}
	}
	return protocol, ip, port, backlog, nil, nil
}

// validateBind accepts only local unicast addresses. An empty bind means the
// IPv4 wildcard, matching the listener options contract.
func validateBind(raw string) (net.IP, error) {
	bind := strings.TrimSpace(raw)
	if bind == "" {
		return net.IPv4zero, nil
	}
	ip := net.ParseIP(bind)
	if ip == nil {
		return nil, fmt.Errorf("bind %q is not an IP address", raw)
	}
	if ip.IsMulticast() {
		return nil, fmt.Errorf("bind %q is a multicast address", raw)
	}
	if ip.Equal(net.IPv4bcast) {
		return nil, fmt.Errorf("bind %q is a broadcast address", raw)
	}
	if isSubnetBroadcast(ip) {
		return nil, fmt.Errorf("bind %q is a broadcast address", raw)
	}
	if ip.IsUnspecified() || ip.IsLoopback() {
		return ip, nil
	}
	if !isLocalUnicast(ip) {
		return nil, fmt.Errorf("bind %q is not a local interface address", raw)
	}
	return ip, nil
}

// isSubnetBroadcast rejects the directed broadcast address of any local
// interface, not just 255.255.255.255.
func isSubnetBroadcast(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok || ipnet.IP.To4() == nil {
			continue
		}
		mask := net.IP(ipnet.Mask).To4()
		if mask == nil {
			continue
		}
		broadcast := make(net.IP, net.IPv4len)
		for i := range broadcast {
			broadcast[i] = ipnet.IP.To4()[i] | ^mask[i]
		}
		if v4.Equal(broadcast) {
			return true
		}
	}
	return false
}

func isLocalUnicast(ip net.IP) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.Equal(ip) {
			return true
		}
	}
	return false
}

func (r *runChannel) listen(protocol string, ip net.IP, port, backlog int, group net.IP) (*listenerAlloc, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("run is shutting down")
	}
	if r.listeners >= MaxListenersPerRun {
		r.mu.Unlock()
		return nil, fmt.Errorf("run already holds the maximum of %d listeners", MaxListenersPerRun)
	}
	r.mu.Unlock()

	var (
		alloc *listenerAlloc
		err   error
	)
	// The pre-check above keeps the common case from opening a socket at all;
	// the authoritative live count is incremented in retainAllocation after the
	// descriptor has actually been handed over.
	_ = alloc
	switch {
	case protocol == "tcp":
		alloc, err = r.listenTCP(ip, port, backlog)
	case group != nil:
		alloc, err = r.listenUDPGroup(group, port)
	default:
		alloc, err = r.listenUDP(ip, port)
	}
	if err != nil {
		return nil, err
	}

	return alloc, nil
}

func (r *runChannel) listenTCP(ip net.IP, port, backlog int) (*listenerAlloc, error) {
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: ip, Port: port})
	if err != nil {
		return nil, err
	}
	// net.ListenTCP has no backlog argument and always listens with a default
	// queue. Re-issue listen(2) with the policy-approved backlog before the fd
	// is duplicated for the app, so the ABI contract is real on the device.
	rc, err := ln.SyscallConn()
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("listener control: %w", err)
	}
	var listenErr error
	if err := rc.Control(func(fd uintptr) { listenErr = unix.Listen(int(fd), backlog) }); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("apply listen backlog: %w", err)
	}
	if listenErr != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("apply listen backlog %d: %w", backlog, listenErr)
	}
	addr := ln.Addr().(*net.TCPAddr)
	file, err := ln.File()
	_ = ln.Close()
	if err != nil {
		return nil, fmt.Errorf("duplicate listening socket: %w", err)
	}
	return &listenerAlloc{file: file, protocol: "tcp", address: addr.IP.String(), port: addr.Port}, nil
}

// listenUDPGroup joins the PTP multicast group on a PTP port. The daemon, which
// runs as root, binds the privileged port once and joins the group on every LAN
// interface; the app receives datagrams the sender addresses to the group, which
// a socket bound to the port alone never sees. Membership lives on the socket, so
// it ends with the transferred descriptor and whatever the daemon still holds.
func (r *runChannel) listenUDPGroup(group net.IP, port int) (*listenerAlloc, error) {
	interfaces := selectMulticastInterfaces(mustInterfaces())
	if len(interfaces) == 0 {
		return nil, errors.New("no multicast-capable network interface is up")
	}
	listenConfig := net.ListenConfig{
		Control: func(_, _ string, raw syscall.RawConn) error {
			var optionErr error
			if err := raw.Control(func(fd uintptr) {
				optionErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
				if optionErr == nil {
					// Best effort: lets a co-resident timing service keep the
					// port while this receiver is restarted.
					_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
				}
			}); err != nil {
				return err
			}
			return optionErr
		},
	}
	packet, err := listenConfig.ListenPacket(context.Background(), "udp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return nil, err
	}
	conn, ok := packet.(*net.UDPConn)
	if !ok {
		_ = packet.Close()
		return nil, errors.New("the kernel did not return a UDP socket")
	}
	groupAddr := &net.UDPAddr{IP: group}
	memberships := ipv4.NewPacketConn(conn)
	joined := 0
	var joinErr error
	for i := range interfaces {
		iface := interfaces[i]
		if err := memberships.JoinGroup(&iface, groupAddr); err != nil {
			joinErr = err
			continue
		}
		joined++
	}
	if joined == 0 {
		_ = packet.Close()
		if joinErr == nil {
			joinErr = errors.New("no interface accepted the group membership")
		}
		return nil, fmt.Errorf("join %s: %w", group, joinErr)
	}
	if r.manager != nil && r.manager.logs != nil {
		r.manager.logs.Infof("listener", r.appID,
			"run %s: joined %s on port %d over %d interface(s)", r.runID, group, port, joined)
	}
	addr := conn.LocalAddr().(*net.UDPAddr)
	file, err := conn.File()
	_ = packet.Close()
	if err != nil {
		return nil, fmt.Errorf("duplicate the multicast socket: %w", err)
	}
	return &listenerAlloc{
		file: file, protocol: "udp", address: addr.IP.String(), port: addr.Port,
		group: group.String(),
	}, nil
}

// mustInterfaces is net.Interfaces with a nil-on-error shape for the selector
// used above; a host without interfaces cannot receive PTP anyway.
func mustInterfaces() []net.Interface {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	return interfaces
}

// selectMulticastInterfaces keeps the interfaces a PTP join should use: up,
// multicast-capable and not loopback. Virtual point-to-point links are kept,
// because a sender may reach the speaker over one.
func selectMulticastInterfaces(interfaces []net.Interface) []net.Interface {
	selected := make([]net.Interface, 0, len(interfaces))
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if iface.Flags&net.FlagMulticast == 0 {
			continue
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		selected = append(selected, iface)
	}
	return selected
}

func (r *runChannel) listenUDP(ip net.IP, port int) (*listenerAlloc, error) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: ip, Port: port})
	if err != nil {
		return nil, err
	}
	addr := conn.LocalAddr().(*net.UDPAddr)
	file, err := conn.File()
	_ = conn.Close()
	if err != nil {
		return nil, fmt.Errorf("duplicate listening socket: %w", err)
	}
	return &listenerAlloc{file: file, protocol: "udp", address: addr.IP.String(), port: addr.Port}, nil
}

func (m *Manager) logf(appID, format string, args ...any) {
	if m.logs != nil {
		m.logs.Infof("appcontrol", appID, format, args...)
	}
}
