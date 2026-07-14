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
	"ephemera/internal/network/quic"
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
//
// The QUIC flow table (reassemblerForQUIC, Task 6c) reuses this same cap for
// its own, wholly separate table: each live QUIC flow buffers at most
// quic.maxQuicClientHelloBytes (8 KiB), so that table's worst case adds
// another 4096 * 8 KiB = 32 MiB per loop, on top of the TCP table above. Same
// LRU-eviction-degrades-to-fail-closed guarantee applies to both tables.
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

// quicFlowEntry is one live multi-datagram QUIC Initial reassembly, keyed by
// "srcIP:sport" — the QUIC counterpart of sniFlowEntry, holding a
// *quic.InitialReassembler (Task 6b) instead of a *sni.Reassembler.
type quicFlowEntry struct {
	key string
	r   *quic.InitialReassembler
}

type sniVerdictLoop struct {
	queueNum  int
	auditPath string         // tenant-scoped runtime audit trail; deny records land here via recordVerdict/auditDeny.
	metrics   *daemonMetrics // allowed/denied SNI verdict counters; nil-safe (see daemonMetrics.IncSNIVerdict).
	connMark  int            // approved conntrack mark applied on sniAcceptMark.

	mu       sync.RWMutex
	registry map[string]sniRegistryEntry // key: guest source IP (dotted quad)
	ready    bool

	// Reassembly LRU. flowMu guards ONLY this flow table (map + list); it is NOT
	// held across the reassembler's Feed call in the Start hook. That Feed runs
	// lock-free and is safe solely because go-nfqueue dispatches the hook from a
	// single goroutine, so at most one Feed touches any *sni.Reassembler at a time.
	// Adding a second concurrent reader (e.g. a worker pool over the hook) would
	// race on the per-flow reassembler state and MUST be preceded by a per-flow
	// guard around Feed — flowMu alone does not make that safe.
	flowMu  sync.Mutex
	flows   map[string]*list.Element // key -> element carrying *sniFlowEntry
	flowLRU *list.List               // front = most recently used

	// QUIC reassembly LRU (multi-datagram Initial CRYPTO reassembly, Task 6c).
	// Mirrors the flowMu/flows/flowLRU triple above but keyed/typed for
	// *quic.InitialReassembler, and is a wholly separate bounded table so a burst
	// of TCP flows can never evict a QUIC flow's reassembler or vice versa. Same
	// discipline as flowMu: flowMuQUIC guards ONLY this table, not the
	// reassembler's Feed call in handleUDPQUIC, which runs lock-free and is safe
	// solely because go-nfqueue dispatches the hook from a single goroutine (see
	// the flowMu field comment above).
	flowMuQUIC  sync.Mutex
	quicFlows   map[string]*list.Element // key -> element carrying *quicFlowEntry
	quicFlowLRU *list.List               // front = most recently used
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

		quicFlows:   map[string]*list.Element{},
		quicFlowLRU: list.New(),
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
	entry, ok := l.resolveEntry(srcIP)
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

// decideQUIC is decide's QUIC/UDP:443 counterpart (also unit-tested without
// root). payload is one UDP datagram believed to carry a QUIC Initial packet;
// srcIP/sport identify the flow, keying the per-flow *quic.InitialReassembler
// this function drives via reassemblerForQUIC (Task 6c).
//
// A modern post-quantum ClientHello (X25519MLKEM768) does not fit in a single
// QUIC Initial datagram, so — mirroring TCP's decide/Start split — this
// function feeds each datagram to the flow's InitialReassembler and mirrors
// TCP's incomplete-segment passthrough rather than terminally denying a
// ClientHello that merely hasn't finished arriving yet:
//
//   - unregistered srcIP  -> sniDrop (reason unregistered_source; no
//     reassembler created)
//   - Feed error           -> sniDrop (reason egress_sni_unparsed) + flow evict
//     (malformed/undecryptable/oversized/no-SNI — all terminal, fail-closed)
//   - Feed incomplete      -> sniDrop (reason egress_sni_incomplete; the datagram
//     is dropped, NOT forwarded, but the flow's reassembler keeps its buffered
//     CRYPTO so the ClientHello still completes across datagrams — see below)
//   - Feed done, in matcher     -> sniAcceptMark (reason egress_sni_allowed) + evict
//   - Feed done, not in matcher -> sniDrop (reason egress_sni_denied, SNI recorded) + evict
//
// Why an incomplete datagram is DROPPED, not passed through (deviation from the
// TCP branch's incomplete-segment passthrough, and from the original Task 6c
// design): a passed-through incomplete Initial reaches the server carrying a
// partial ClientHello AND — being the flow's first accepted packet — confirms
// the UDP conntrack entry with mark 0. The connmark then set on the *completing*
// (second) datagram loses the race with that already-confirmed entry, so the
// -sni-udp-fastpath rule never matches and every later (non-Initial) datagram of
// an ALLOWED flow re-queues here, fails to decrypt as an Initial, and is dropped
// — breaking the handshake for every multi-datagram (post-quantum) ClientHello.
// Dropping the incomplete datagram instead (a) makes the completing, allowed
// datagram the FIRST accepted packet so its connmark confirms cleanly and the
// fast path engages, and (b) never leaks a partial ClientHello to the server: the
// client retransmits the dropped datagram via QUIC loss recovery once the flow is
// allowed and marked, and that retransmit rides the fast path. It is strictly
// fail-closed — nothing reaches the server until the whole ClientHello is
// reassembled and the SNI is allowed. The reassembler still accumulates across
// datagrams (Feed runs before the drop verdict), bounded by its per-flow byte cap
// and the QUIC flow LRU; an incomplete datagram can never yield an approved
// connmark.
func (l *sniVerdictLoop) decideQUIC(srcIP string, sport uint16, payload []byte) sniDecision {
	entry, ok := l.resolveEntry(srcIP)
	if !ok {
		return sniDecision{Action: sniDrop, Reason: "unregistered_source"}
	}
	flowKey := srcIP + ":" + strconv.Itoa(int(sport))
	r := l.reassemblerForQUIC(flowKey)
	name, done, ferr := r.Feed(payload)
	switch {
	case ferr != nil:
		// Terminal parse error (malformed / no-SNI / inconsistent overlap /
		// oversized) -> fail closed.
		l.evictQUICFlow(flowKey)
		return sniDecision{Action: sniDrop, Reason: "egress_sni_unparsed"}
	case !done:
		// Incomplete ClientHello: fail closed (drop, no RST for UDP) rather than
		// forward a partial ClientHello to the server. The flow stays cached so
		// the next Initial datagram accumulates onto the same reassembler; the
		// client retransmits this dropped datagram once the flow is allowed and
		// the connmark fast path is in place. See the doc comment above.
		return sniDecision{Action: sniDrop, Reason: "egress_sni_incomplete"}
	default:
		l.evictQUICFlow(flowKey)
		return l.classifyParsedSNI(entry, name) // shared with TCP: in matcher -> acceptMark, else denied
	}
}

// resolveEntry does the locked registry lookup that both decide (single-packet
// core) and the Start hook's pre-classify stage share, so the RLock/RUnlock dance
// and the unregistered-source fail-closed contract live in exactly one place. It
// only reads the registry; callers own the drop/passthrough/parse routing that
// follows, which differs between the pure core (returns a decision) and the hook
// (issues kernel verdicts + drives the reassembler).
func (l *sniVerdictLoop) resolveEntry(srcIP string) (sniRegistryEntry, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	entry, ok := l.registry[srcIP]
	return entry, ok
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
// evicting the least-recently-used flow when the table is at capacity. flowMu is
// released before the returned reassembler's Feed runs in the hook; that is safe
// only under single-goroutine hook dispatch (see the flowMu field comment) — a
// parallel reader would need a per-flow guard around Feed first.
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

// reassemblerForQUIC is reassemblerFor's QUIC counterpart: it returns the
// flow's *quic.InitialReassembler, creating one if absent and evicting the
// least-recently-used flow once this (separate) table is at
// sniReassemblerMaxFlows capacity — the flow-count cap a malicious guest
// cannot bypass by opening many QUIC flows that each dribble a
// never-completing Initial (per-flow bytes are separately capped by
// InitialReassembler itself, at quic.maxQuicClientHelloBytes; see Task 6b).
// flowMuQUIC is released before the returned reassembler's Feed runs in
// handleUDPQUIC; that is safe only under single-goroutine hook dispatch (see
// flowMuQUIC's field comment).
func (l *sniVerdictLoop) reassemblerForQUIC(key string) *quic.InitialReassembler {
	l.flowMuQUIC.Lock()
	defer l.flowMuQUIC.Unlock()
	if el, ok := l.quicFlows[key]; ok {
		l.quicFlowLRU.MoveToFront(el)
		return el.Value.(*quicFlowEntry).r
	}
	for l.quicFlowLRU.Len() >= sniReassemblerMaxFlows {
		back := l.quicFlowLRU.Back()
		if back == nil {
			break
		}
		l.quicFlowLRU.Remove(back)
		delete(l.quicFlows, back.Value.(*quicFlowEntry).key)
	}
	r := &quic.InitialReassembler{}
	l.quicFlows[key] = l.quicFlowLRU.PushFront(&quicFlowEntry{key: key, r: r})
	return r
}

func (l *sniVerdictLoop) evictQUICFlow(key string) {
	l.flowMuQUIC.Lock()
	defer l.flowMuQUIC.Unlock()
	if el, ok := l.quicFlows[key]; ok {
		l.quicFlowLRU.Remove(el)
		delete(l.quicFlows, key)
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

// ipv4UDP is the subset of an IPv4/UDP packet the verdict hook needs. Unlike
// ipv4TCP it carries no seq/ackSeq: UDP has no sequence numbers, so a UDP drop
// can never inject a credible RST (see setDropNoRST).
type ipv4UDP struct {
	srcIP, dstIP net.IP // srcIP = guest, dstIP = server (dport 443)
	sport, dport uint16
	payload      []byte
}

// parseIPv4UDP parses an IPv4/UDP packet, the UDP counterpart of parseIPv4TCP.
// It bounds the returned payload by the UDP header's own Length field (not
// merely by the remaining packet bytes), so IP-level padding or a truncated
// datagram can never leak trailing garbage into the payload quic.ParseInitialSNI
// inspects.
func parseIPv4UDP(pkt []byte) (ipv4UDP, error) {
	var u ipv4UDP
	if len(pkt) < 20 {
		return u, fmt.Errorf("short ip packet (%d bytes)", len(pkt))
	}
	if pkt[0]>>4 != 4 {
		return u, fmt.Errorf("not ipv4 (version %d)", pkt[0]>>4)
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl {
		return u, fmt.Errorf("bad ihl %d", ihl)
	}
	if pkt[9] != unix.IPPROTO_UDP {
		return u, fmt.Errorf("not udp (proto %d)", pkt[9])
	}
	u.srcIP = net.IPv4(pkt[12], pkt[13], pkt[14], pkt[15])
	u.dstIP = net.IPv4(pkt[16], pkt[17], pkt[18], pkt[19])
	udpHdr := pkt[ihl:]
	if len(udpHdr) < 8 {
		return u, fmt.Errorf("short udp header (%d bytes)", len(udpHdr))
	}
	u.sport = binary.BigEndian.Uint16(udpHdr[0:2])
	u.dport = binary.BigEndian.Uint16(udpHdr[2:4])
	udpLen := int(binary.BigEndian.Uint16(udpHdr[4:6]))
	if udpLen < 8 || udpLen > len(udpHdr) {
		return u, fmt.Errorf("bad udp length %d (have %d)", udpLen, len(udpHdr))
	}
	u.payload = udpHdr[8:udpLen]
	return u, nil
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
			l.metrics.IncSNIVerdict("dropped") // pre-classify infra drop (distinct from policy denial)
			return 0
		}
		pkt := *a.Payload
		if len(pkt) > 9 && pkt[9] == unix.IPPROTO_UDP {
			// QUIC/UDP:443 path: per-flow multi-datagram Initial reassembly via
			// decideQUIC's flow cache — see handleUDPQUIC.
			l.handleUDPQUIC(nf, id, pkt)
			return 0
		}
		t, perr := parseIPv4TCP(pkt)
		if perr != nil {
			slog.Debug("sni verdict: unparsable packet, fail-closed drop", "err", perr)
			l.setDrop(nf, id, t)
			l.metrics.IncSNIVerdict("dropped") // pre-classify infra drop (distinct from policy denial)
			return 0
		}
		srcIP := t.srcIP.String()

		entry, ok := l.resolveEntry(srcIP)
		if !ok {
			// Defense in depth: the iptables dispatch rule is already scoped by
			// -s guestIP, but an unregistered source still fails closed here.
			l.setDrop(nf, id, t)
			l.metrics.IncSNIVerdict("dropped") // pre-classify infra drop (distinct from policy denial)
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

// handleUDPQUIC is the UDP:443 (QUIC) counterpart of the TCP branch of the
// hook body above. Like TCP's ClientHello, a QUIC Initial's SNI may span more
// than one datagram (a modern post-quantum ClientHello does not fit in one
// Initial), so this hook feeds each datagram to the flow's
// *quic.InitialReassembler via decideQUIC — which owns the flow lookup,
// eviction, and passthrough/accept/drop routing (Task 6c) — mirroring the TCP
// branch's reassemblerFor/Feed/evictFlow shape. The hook body itself still
// reduces to parse -> resolve -> decideQUIC -> apply; the incomplete/re-queue
// branch now lives inside decideQUIC instead of being absent.
//
// Every early-return here (parse failure, wrong dport, unregistered source) is
// a pre-classify infra drop: it fails closed via setDropNoRST + a "dropped"
// metric, bypassing recordVerdict, exactly mirroring the TCP branch's
// pre-classify drops above (see their "distinct from policy denial" comments).
// Only a resolved decideQUIC verdict goes through applyVerdictUDP/recordVerdict.
func (l *sniVerdictLoop) handleUDPQUIC(nf *nfqueue.Nfqueue, id uint32, pkt []byte) {
	u, perr := parseIPv4UDP(pkt)
	if perr != nil {
		slog.Debug("sni verdict: unparsable udp packet, fail-closed drop", "err", perr)
		l.setDropNoRST(nf, id)
		l.metrics.IncSNIVerdict("dropped")
		return
	}
	if u.dport != 443 {
		// Defense in depth: the iptables dispatch rule is expected to already
		// scope this queue to dport 443, but fail closed here too rather than
		// trust that blindly.
		l.setDropNoRST(nf, id)
		l.metrics.IncSNIVerdict("dropped")
		return
	}
	srcIP := u.srcIP.String()
	entry, ok := l.resolveEntry(srcIP)
	if !ok {
		// Defense in depth: the iptables dispatch rule is already scoped by
		// -s guestIP, but an unregistered source still fails closed here.
		l.setDropNoRST(nf, id)
		l.metrics.IncSNIVerdict("dropped")
		return
	}
	d := l.decideQUIC(srcIP, u.sport, u.payload)
	slog.Debug("sni verdict: quic udp datagram",
		"flow", srcIP+":"+strconv.Itoa(int(u.sport)),
		"payload_len", len(u.payload),
		"action", d.Action.String(),
		"reason", d.Reason)
	l.applyVerdictUDP(nf, id, d, entry)
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

// applyVerdictUDP is applyVerdict's UDP/QUIC counterpart: identical
// accept/accept+mark/passthrough semantics, but a UDP drop never attempts
// injectRST — that needs the TCP seq/ackSeq ipv4TCP carries (RFC 793 has no
// UDP analog), so setDropNoRST issues a plain fail-closed NfDrop instead.
// recordVerdict (metrics/audit) is shared unchanged with the TCP path.
func (l *sniVerdictLoop) applyVerdictUDP(nf *nfqueue.Nfqueue, id uint32, d sniDecision, entry sniRegistryEntry) {
	// Multi-datagram QUIC Initial reassembly (Task 6c, revised Task 7b): while a
	// flow's ClientHello is still incomplete, decideQUIC yields a fail-closed
	// sniDrop (reason egress_sni_incomplete) — the datagram is dropped, not
	// forwarded, so no partial ClientHello reaches the server and the completing
	// datagram becomes the flow's first accepted packet (its connmark then
	// confirms cleanly onto the conntrack entry). The client retransmits the
	// dropped datagram once the flow is allowed and the fast path is in place.
	switch d.Action {
	case sniAcceptMark:
		if err := nf.SetVerdictWithConnMark(id, nfqueue.NfAccept, l.connMark); err != nil {
			slog.Error("sni verdict: set accept+connmark failed", "err", err, "sni", d.SNI)
		}
	case sniPassthrough:
		l.setAccept(nf, id)
	case sniDrop:
		l.setDropNoRST(nf, id)
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
		if d.Reason == "egress_sni_incomplete" {
			// Fail-closed buffering drop of an incomplete multi-datagram Initial
			// (see decideQUIC): the ClientHello has not been classified yet, so
			// this is neither an allow nor a policy deny. The client retransmits
			// the datagram once the flow completes, so counting it as "denied"
			// (or auditing it) would double-count a still-in-flight handshake.
			return
		}
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

// setDropNoRST issues a fail-closed DROP with no RST attempt — the UDP/QUIC
// counterpart of setDrop. injectRST is TCP-specific (it spoofs a RST using the
// guest segment's ack_seq, RFC 793); UDP has no sequence numbers to build a
// credible RST from, so this path silently drops and lets the guest's own QUIC
// idle timeout tear the connection down, same fail-closed-never-fail-open
// guarantee as setDrop, just without the fast-fail courtesy.
func (l *sniVerdictLoop) setDropNoRST(nf *nfqueue.Nfqueue, id uint32) {
	if err := nf.SetVerdict(id, nfqueue.NfDrop); err != nil {
		slog.Error("sni verdict: set drop failed", "err", err)
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
