package main

import (
	"container/list"
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"ephemera/internal/anvilmcp"
	"ephemera/internal/network/sni"

	nfqueue "github.com/florianl/go-nfqueue/v2"
	"golang.org/x/sys/unix"
)

// sniReassemblerMaxFlows bounds the per-loop reassembly table so a malicious
// guest cannot exhaust host memory by opening many TLS flows that each dribble a
// never-completing ClientHello. Each live flow buffers at most
// sni.maxClientHelloBytes (16 KiB), so the worst-case ceiling is
// 4096 * 16 KiB = 64 MiB per loop. ClientHellos are almost always a single
// packet, so only genuinely multi-segment (or attacker-stalled) flows ever hold
// a slot, and each is evicted the moment it completes or hits a terminal parse
// error. When the table is full a new flow evicts the least-recently-used entry
// (LRU); the evicted flow degrades to fail-closed (its next segment starts a
// fresh reassembler mid-stream, fails to parse as a record boundary, and drops)
// — never to fail-open.
const sniReassemblerMaxFlows = 4096

type sniAction int

const (
	sniPassthrough sniAction = iota // ACCEPT, no conntrack mark (handshake/non-ClientHello)
	sniAcceptMark                   // ACCEPT + set approved conntrack mark
	sniDrop                         // DROP (+ best-effort RST) — fail-closed
)

func (a sniAction) String() string {
	switch a {
	case sniPassthrough:
		return "passthrough"
	case sniAcceptMark:
		return "accept_mark"
	case sniDrop:
		return "drop"
	default:
		return "unknown"
	}
}

type sniDecision struct {
	Action sniAction
	SNI    string
	Reason string
}

type sniRegistryEntry struct {
	VMID     string
	TenantID string
	Profile  string
	Matcher  *sni.Matcher
}

// sniFlowEntry is one live multi-segment reassembly, keyed by "guestIP:sport".
type sniFlowEntry struct {
	key string
	r   *sni.Reassembler
}

type sniVerdictLoop struct {
	queueNum  int
	auditPath string         // tenant-scoped runtime audit trail; deny records land here via recordVerdict/auditDeny.
	metrics   *daemonMetrics // allowed/denied SNI verdict counters; nil-safe (see daemonMetrics.IncSNIVerdict).
	connMark  int            // approved conntrack mark applied on sniAcceptMark.

	mu       sync.RWMutex
	registry map[string]sniRegistryEntry // key: guest source IP (dotted quad)
	ready    bool

	// Reassembly LRU. Touched only from the single hook goroutine in Start, but
	// guarded so Start could be extended to multiple readers without a data race.
	flowMu  sync.Mutex
	flows   map[string]*list.Element // key -> element carrying *sniFlowEntry
	flowLRU *list.List               // front = most recently used
}

func newSNIVerdictLoop(queueNum int, auditPath string, metrics *daemonMetrics) *sniVerdictLoop {
	return &sniVerdictLoop{
		queueNum:  queueNum,
		auditPath: auditPath,
		metrics:   metrics,
		connMark:  mustParseConnmark(sniApprovedConnmark),
		registry:  map[string]sniRegistryEntry{},
		flows:     map[string]*list.Element{},
		flowLRU:   list.New(),
	}
}

// mustParseConnmark parses the approved-connmark constant ("0x534e49") into the
// int the go-nfqueue verdict API expects. The argument is a compile-time
// constant, so a parse failure is a programming error and panics at startup.
func mustParseConnmark(s string) int {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 0, 32)
	if err != nil {
		panic(fmt.Sprintf("sni verdict loop: invalid connmark %q: %v", s, err))
	}
	return int(v)
}

func (l *sniVerdictLoop) Register(guestIP string, e sniRegistryEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.registry[guestIP] = e
}

func (l *sniVerdictLoop) Deregister(guestIP string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.registry, guestIP)
}

func (l *sniVerdictLoop) Ready() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.ready
}

// decide is the pure routing core (unit-tested without root). payload is the TCP
// application payload of one queued packet; srcIP is the packet source address.
//
// Fail-closed contract:
//   - unregistered srcIP        -> sniDrop  (reason unregistered_source)
//   - empty payload (handshake) -> sniPassthrough (ACCEPT, no mark; next segment re-queues)
//   - any parse error           -> sniDrop  (reason egress_sni_unparsed)
//   - SNI in matcher            -> sniAcceptMark (reason egress_sni_allowed)
//   - SNI not in matcher        -> sniDrop  (reason egress_sni_denied, SNI recorded)
//
// NOTE (deviation from brief Step 3 case (c)): the brief's pseudocode mapped
// sni.ErrIncomplete to sniPassthrough. We instead fail closed on *every* parse
// error including ErrIncomplete, because (1) the verbatim malformed test vector
// {0x16,0x03,0x01,0xff,0xff,0x01} declares a 64 KiB record in a 6-byte buffer and
// so classifies as ErrIncomplete, yet must DROP; and (2) forwarding an
// unverifiable single packet is strictly less safe than dropping it. Legitimate
// multi-segment ClientHellos are reassembled in Start (which owns the "need more
// bytes" signal via sni.Reassembler) before any decision is taken, so decide
// never has to say "retry" — the loop, not the classifier, re-queues partials.
func (l *sniVerdictLoop) decide(srcIP string, payload []byte) sniDecision {
	l.mu.RLock()
	entry, ok := l.registry[srcIP]
	l.mu.RUnlock()
	if !ok {
		return sniDecision{Action: sniDrop, Reason: "unregistered_source"}
	}
	if len(payload) == 0 {
		return sniDecision{Action: sniPassthrough} // handshake packet, let it through unmarked
	}
	name, err := sni.ParseClientHelloSNI(payload)
	if err != nil {
		return sniDecision{Action: sniDrop, Reason: "egress_sni_unparsed"} // fail-closed
	}
	return l.classifyParsedSNI(entry, name)
}

// classifyParsedSNI applies the allow-list policy to an already-parsed SNI for a
// registered source. Shared by decide (single-packet fast path) and Start's
// multi-segment reassembly path so the accept/deny rule lives in exactly one place.
func (l *sniVerdictLoop) classifyParsedSNI(entry sniRegistryEntry, name string) sniDecision {
	if entry.Matcher.Match(name) {
		return sniDecision{Action: sniAcceptMark, SNI: name, Reason: "egress_sni_allowed"}
	}
	return sniDecision{Action: sniDrop, SNI: name, Reason: "egress_sni_denied"}
}

// reassemblerFor returns the flow's reassembler, creating one if absent and
// evicting the least-recently-used flow when the table is at capacity.
func (l *sniVerdictLoop) reassemblerFor(key string) *sni.Reassembler {
	l.flowMu.Lock()
	defer l.flowMu.Unlock()
	if el, ok := l.flows[key]; ok {
		l.flowLRU.MoveToFront(el)
		return el.Value.(*sniFlowEntry).r
	}
	for l.flowLRU.Len() >= sniReassemblerMaxFlows {
		back := l.flowLRU.Back()
		if back == nil {
			break
		}
		l.flowLRU.Remove(back)
		delete(l.flows, back.Value.(*sniFlowEntry).key)
	}
	r := &sni.Reassembler{}
	l.flows[key] = l.flowLRU.PushFront(&sniFlowEntry{key: key, r: r})
	return r
}

func (l *sniVerdictLoop) evictFlow(key string) {
	l.flowMu.Lock()
	defer l.flowMu.Unlock()
	if el, ok := l.flows[key]; ok {
		l.flowLRU.Remove(el)
		delete(l.flows, key)
	}
}

// ipv4TCP is the subset of an IPv4/TCP packet the verdict hook needs.
type ipv4TCP struct {
	srcIP, dstIP net.IP // srcIP = guest, dstIP = server (dport 443)
	sport, dport uint16
	seq, ackSeq  uint32
	payload      []byte
}

func parseIPv4TCP(pkt []byte) (ipv4TCP, error) {
	var t ipv4TCP
	if len(pkt) < 20 {
		return t, fmt.Errorf("short ip packet (%d bytes)", len(pkt))
	}
	if pkt[0]>>4 != 4 {
		return t, fmt.Errorf("not ipv4 (version %d)", pkt[0]>>4)
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl {
		return t, fmt.Errorf("bad ihl %d", ihl)
	}
	if pkt[9] != unix.IPPROTO_TCP {
		return t, fmt.Errorf("not tcp (proto %d)", pkt[9])
	}
	t.srcIP = net.IPv4(pkt[12], pkt[13], pkt[14], pkt[15])
	t.dstIP = net.IPv4(pkt[16], pkt[17], pkt[18], pkt[19])
	tcp := pkt[ihl:]
	if len(tcp) < 20 {
		return t, fmt.Errorf("short tcp header (%d bytes)", len(tcp))
	}
	t.sport = binary.BigEndian.Uint16(tcp[0:2])
	t.dport = binary.BigEndian.Uint16(tcp[2:4])
	t.seq = binary.BigEndian.Uint32(tcp[4:8])
	t.ackSeq = binary.BigEndian.Uint32(tcp[8:12])
	dataOff := int(tcp[12]>>4) * 4
	if dataOff < 20 || len(tcp) < dataOff {
		return t, fmt.Errorf("bad tcp data offset %d", dataOff)
	}
	t.payload = tcp[dataOff:]
	return t, nil
}

// Start binds the NFQUEUE listener and registers the verdict hook. It is a
// preflight: on nfqueue.Open failure it leaves Ready()==false and returns the
// error, never silently pretending to have installed a filter. Note the config
// intentionally does NOT set NfQaCfgFlagFailOpen, so if this listener is absent
// or dies the kernel DROPs queued packets (fail-closed) rather than letting them
// bypass inspection.
//
// This whole path (netlink bind, verdict I/O, connmark, RST) needs root +
// netfilter and is verified by the Task 7 KVM e2e; the unit tests cover only
// decide()/the registry/fail-closed routing.
func (l *sniVerdictLoop) Start(ctx context.Context) error {
	cfg := &nfqueue.Config{
		NfQueue:      uint16(l.queueNum),
		MaxPacketLen: 0xffff,
		MaxQueueLen:  0xff,
		Copymode:     nfqueue.NfQnlCopyPacket,
		Flags:        nfqueue.NfQaCfgFlagConntrack, // conntrack info for connmark; NOT FailOpen -> kernel drops with no listener
		WriteTimeout: 15 * time.Millisecond,
	}
	nf, err := nfqueue.Open(cfg)
	if err != nil {
		l.mu.Lock()
		l.ready = false
		l.mu.Unlock()
		return fmt.Errorf("sni verdict loop: open nfqueue %d: %w", l.queueNum, err)
	}

	hook := func(a nfqueue.Attribute) int {
		if a.PacketID == nil {
			return 0
		}
		id := *a.PacketID
		if a.Payload == nil {
			// No packet bytes to inspect -> cannot verify -> fail closed.
			l.setDrop(nf, id, ipv4TCP{})
			return 0
		}
		t, perr := parseIPv4TCP(*a.Payload)
		if perr != nil {
			slog.Debug("sni verdict: unparsable packet, fail-closed drop", "err", perr)
			l.setDrop(nf, id, t)
			return 0
		}
		srcIP := t.srcIP.String()

		l.mu.RLock()
		entry, ok := l.registry[srcIP]
		l.mu.RUnlock()
		if !ok {
			// Defense in depth: the iptables dispatch rule is already scoped by
			// -s guestIP, but an unregistered source still fails closed here.
			l.setDrop(nf, id, t)
			return 0
		}
		if len(t.payload) == 0 {
			// Handshake / bare ACK: no TLS bytes yet, pass unmarked so a later
			// ClientHello segment re-queues (connmark stays unset).
			l.setAccept(nf, id)
			return 0
		}

		flowKey := srcIP + ":" + strconv.Itoa(int(t.sport))
		r := l.reassemblerFor(flowKey)
		name, done, ferr := r.Feed(t.payload)
		switch {
		case ferr != nil:
			// Terminal parse error (malformed / no-SNI / oversized) -> fail closed.
			l.evictFlow(flowKey)
			l.applyVerdict(nf, id, sniDecision{Action: sniDrop, Reason: "egress_sni_unparsed"}, t, entry)
		case !done:
			// Incomplete ClientHello: forward this segment unmarked so the next
			// segment re-queues here. Bounded by the reassembler's 16 KiB buffer
			// and the LRU flow cap; it can never yield an approved connmark.
			l.setAccept(nf, id)
		default:
			l.evictFlow(flowKey)
			l.applyVerdict(nf, id, l.classifyParsedSNI(entry, name), t, entry)
		}
		return 0
	}

	errHook := func(e error) int {
		// Netlink read error: log and keep the loop alive (return 0 = continue).
		slog.Error("sni verdict loop: netlink error", "err", e)
		return 0
	}

	if err := nf.RegisterWithErrorFunc(ctx, hook, errHook); err != nil {
		_ = nf.Close()
		l.mu.Lock()
		l.ready = false
		l.mu.Unlock()
		return fmt.Errorf("sni verdict loop: register hook on queue %d: %w", l.queueNum, err)
	}

	l.mu.Lock()
	l.ready = true
	l.mu.Unlock()

	go func() {
		<-ctx.Done()
		_ = nf.Close()
		l.mu.Lock()
		l.ready = false
		l.mu.Unlock()
	}()
	return nil
}

// applyVerdict turns a decision into a kernel verdict. sniAcceptMark is the only
// path that sets the approved connmark; every drop path is best-effort RST +
// unconditional DROP. The kernel verdict is issued first and unconditionally;
// recordVerdict's metric/audit side effects run after and can never delay or
// change it (entry is the registry snapshot already resolved by the caller,
// carried through only for the deny audit's VMID/TenantID).
func (l *sniVerdictLoop) applyVerdict(nf *nfqueue.Nfqueue, id uint32, d sniDecision, t ipv4TCP, entry sniRegistryEntry) {
	switch d.Action {
	case sniAcceptMark:
		if err := nf.SetVerdictWithConnMark(id, nfqueue.NfAccept, l.connMark); err != nil {
			slog.Error("sni verdict: set accept+connmark failed", "err", err, "sni", d.SNI)
		}
	case sniPassthrough:
		l.setAccept(nf, id)
	case sniDrop:
		l.setDrop(nf, id, t)
	}
	l.recordVerdict(entry, d)
}

// recordVerdict emits the audit/metric side effects for a completed SNI
// verdict. It never touches the kernel verdict itself (already issued by
// applyVerdict's caller), so it is pure enough to unit test without root or a
// live nfqueue socket.
//
// Metric: outcome-only counter, incremented unconditionally for both
// accept and drop verdicts, independent of tenant availability or the audit
// write's success/failure — a content-free signal never gated on redaction
// concerns.
//
// Audit: only the sniDrop / "egress_sni_denied" case carries a domain worth
// recording (unregistered_source and egress_sni_unparsed drops have no SNI to
// audit). See auditDeny for the tenant seam.
func (l *sniVerdictLoop) recordVerdict(entry sniRegistryEntry, d sniDecision) {
	switch d.Action {
	case sniAcceptMark:
		l.metrics.IncSNIVerdict("allowed")
	case sniDrop:
		l.metrics.IncSNIVerdict("denied")
		if d.Reason == "egress_sni_denied" {
			l.auditDeny(entry, d)
		}
	}
}

// auditDeny records a fail-closed SNI drop into the tenant-scoped runtime
// audit trail (the same RuntimeAuditRecord stream/redaction contract used by
// anvilmcp's tool-call audit, tenant_policy.go:222). AppendRuntimeAudit
// rejects records with an empty tenant, so a VM whose registry entry carries
// no TenantID degrades to a redaction-safe slog line instead of silently
// losing the signal. Only the SNI domain and VM/tenant identifiers are
// logged — never tokens, authorization, or call args. A write failure
// (including an unset l.auditPath) also degrades to slog rather than
// panicking; the kernel verdict has already been applied by the time this
// runs, so an audit failure can never flip allow/deny.
func (l *sniVerdictLoop) auditDeny(entry sniRegistryEntry, d sniDecision) {
	if entry.TenantID == "" {
		slog.Warn("egress sni denied", "vm_id", entry.VMID, "sni", d.SNI)
		return
	}
	if err := anvilmcp.AppendRuntimeAudit(l.auditPath, anvilmcp.RuntimeAuditRecord{
		TenantID:        entry.TenantID,
		VMID:            entry.VMID,
		ToolName:        "egress_sni_filter",
		DaemonOperation: "egress_sni_denied",
		ResultCode:      "denied",
		SNI:             d.SNI,
	}); err != nil {
		slog.Warn("egress sni deny audit append failed", "vm_id", entry.VMID, "err", err)
	}
}

func (l *sniVerdictLoop) setAccept(nf *nfqueue.Nfqueue, id uint32) {
	if err := nf.SetVerdict(id, nfqueue.NfAccept); err != nil {
		slog.Error("sni verdict: set accept failed", "err", err)
	}
}

// setDrop issues the fail-closed DROP and then best-effort injects a spoofed RST
// toward the guest. RST is a courtesy fast-fail only: if injection fails the
// packet is still dropped, so the guest merely hangs until its own TCP timeout —
// we degrade to a silent drop, never to accept.
func (l *sniVerdictLoop) setDrop(nf *nfqueue.Nfqueue, id uint32, t ipv4TCP) {
	if err := nf.SetVerdict(id, nfqueue.NfDrop); err != nil {
		slog.Error("sni verdict: set drop failed", "err", err)
	}
	if t.srcIP == nil {
		return // no packet context (e.g. missing payload) -> nothing to RST
	}
	if err := l.injectRST(t); err != nil {
		slog.Debug("sni verdict: RST injection failed; guest will time out", "err", err)
	}
}

// injectRST sends a spoofed TCP RST as if from the server (dstIP:dport, i.e.
// :443) to the guest (srcIP:sport), seq = the guest segment's ack_seq, so the
// guest tears the connection down immediately instead of waiting for a timeout.
// Best-effort (see setDrop); real behavior is verified by the Task 7 e2e.
func (l *sniVerdictLoop) injectRST(t ipv4TCP) error {
	server := t.dstIP.To4()
	guest := t.srcIP.To4()
	if server == nil || guest == nil {
		return fmt.Errorf("non-ipv4 addresses, cannot RST")
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_RAW, unix.IPPROTO_TCP)
	if err != nil {
		return fmt.Errorf("raw socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_HDRINCL, 1); err != nil {
		return fmt.Errorf("IP_HDRINCL: %w", err)
	}
	pkt := buildRSTPacket(server, guest, t.dport, t.sport, t.ackSeq)
	var addr unix.SockaddrInet4
	copy(addr.Addr[:], guest)
	addr.Port = int(t.sport)
	if err := unix.Sendto(fd, pkt, 0, &addr); err != nil {
		return fmt.Errorf("sendto: %w", err)
	}
	return nil
}

// buildRSTPacket assembles a 40-byte IPv4+TCP RST (no options) from srcIP:srcPort
// (the server) to dstIP:dstPort (the guest) carrying sequence number seq.
func buildRSTPacket(srcIP, dstIP net.IP, srcPort, dstPort uint16, seq uint32) []byte {
	ip := make([]byte, 20)
	ip[0] = 0x45 // version 4, IHL 5
	binary.BigEndian.PutUint16(ip[2:4], 40)
	ip[8] = 64 // TTL
	ip[9] = unix.IPPROTO_TCP
	copy(ip[12:16], srcIP.To4())
	copy(ip[16:20], dstIP.To4())
	binary.BigEndian.PutUint16(ip[10:12], checksum(ip))

	tcp := make([]byte, 20)
	binary.BigEndian.PutUint16(tcp[0:2], srcPort)
	binary.BigEndian.PutUint16(tcp[2:4], dstPort)
	binary.BigEndian.PutUint32(tcp[4:8], seq)
	tcp[12] = 5 << 4 // data offset 5 words, no options
	tcp[13] = 0x04   // RST
	// pseudo-header (src, dst, zero, proto, tcp length) for the TCP checksum.
	pseudo := make([]byte, 12+len(tcp))
	copy(pseudo[0:4], srcIP.To4())
	copy(pseudo[4:8], dstIP.To4())
	pseudo[9] = unix.IPPROTO_TCP
	binary.BigEndian.PutUint16(pseudo[10:12], uint16(len(tcp)))
	copy(pseudo[12:], tcp)
	binary.BigEndian.PutUint16(tcp[16:18], checksum(pseudo))

	return append(ip, tcp...)
}

// checksum is the 16-bit one's-complement Internet checksum (RFC 1071).
func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}
