package network

// 학습 주석 개요: v0.6.1 에서 도입된 anti-spoof 표면. EPHEMERA_NET_ANTISPOOF 는
// 기본 on 이고, 명시적으로 falsey 값을 줘야만 꺼진다(antiSpoofEnabledFromEnv).
// ebtables 가 PATH 에 없으면(ebtablesAvailable) 이 기능 전체가 best-effort 로
// 동작하지 않을 뿐 daemon 기동을 막지는 않는다 — 즉 "있으면 강제, 없으면 조용히
// 생략"이 이 파일의 실패 모드다. 이 anti-spoof 는 internal/mcpgateway/identity.go
// 가 신뢰하는 "source IP == VM 신원" 가정을 네트워크 계층에서 강화하는
// defense-in-depth 다: TAP 포트를 daemon 이 할당한 MAC+IP 쌍에 고정해, 같은
// 브리지의 다른 VM 이 그 IP 를 스푸핑해 gateway 호출자로 위장하는 것을 막는다.

import (
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

// antiSpoofChain is the dedicated ebtables filter chain holding the per-TAP
// source MAC/IP pins. Keeping our rules in one chain (jumped to from FORWARD and
// INPUT) isolates them from any other ebtables usage and makes startup cleanup a
// single flush.
const antiSpoofChain = "EPHEMERA_AS"

// antiSpoofEnabledFromEnv reports whether per-TAP anti-spoof is on. It defaults
// to enabled; EPHEMERA_NET_ANTISPOOF set to a falsey value opts out.
// 학습 주석: switch 의 default 분기가 없다 — "0/false/no/off" 이외의 모든 값
// (오타 포함)은 그냥 계속 enabled 로 남는다. fail-closed 방향의 기본값 설계.
func antiSpoofEnabledFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("EPHEMERA_NET_ANTISPOOF"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// ebtablesAvailable reports whether the ebtables binary is on PATH.
// 학습 주석: 이 결과에 따라 anti-spoof 규칙 설치 자체를 건너뛴다 — 즉 "best-effort"
// 라는 표현은 이 함수가 false 를 돌려주는 host 에서는 스푸핑 방어가 아예 없다는
// 뜻이다(핸드오프 문서의 "운영 주의" 절 참고, MCP gateway 의 source-IP 403 판정과
// 별개로 필요한 계층).
func ebtablesAvailable() bool {
	_, err := exec.LookPath("ebtables")
	return err == nil
}

// setupAntiSpoofChain creates (if absent) and flushes the EPHEMERA_AS chain, then
// idempotently wires jumps into it from FORWARD and INPUT. Flushing at startup
// drops stale per-TAP rules left by a prior daemon run — the bridge persists
// across restarts, but the daemon's VM view is rebuilt, so recycled-TAP rules
// would otherwise accumulate and mis-pin. Best-effort: failures are logged,
// never fatal (matching setupBridge).
// 학습 주석: 전용 체인(EPHEMERA_AS)에 규칙을 모으고 FORWARD/INPUT 에서 그 체인
// 으로 jump 만 거는 구조라, 다른 ebtables 사용자(운영자가 직접 추가한 규칙 등)
// 와 충돌하지 않는다.
func (m *Manager) setupAntiSpoofChain() {
	// Create the chain only if it does not already exist (-N errors otherwise).
	if exec.Command("ebtables", "-t", "filter", "-L", antiSpoofChain).Run() != nil {
		if err := exec.Command("ebtables", "-t", "filter", "-N", antiSpoofChain).Run(); err != nil {
			slog.Warn("anti-spoof chain create failed", "chain", antiSpoofChain, "err", err)
		}
	}
	// Flush stale per-TAP rules from a prior run.
	exec.Command("ebtables", "-t", "filter", "-F", antiSpoofChain).Run()
	// Idempotently jump into the chain from FORWARD and INPUT (-C checks, -A adds).
	for _, base := range []string{"FORWARD", "INPUT"} {
		if exec.Command("ebtables", "-t", "filter", "-C", base, "-j", antiSpoofChain).Run() != nil {
			if err := exec.Command("ebtables", "-t", "filter", "-A", base, "-j", antiSpoofChain).Run(); err != nil {
				slog.Warn("anti-spoof jump install failed", "from", base, "chain", antiSpoofChain, "err", err)
			}
		}
	}
}

// antiSpoofRuleSets returns the ebtables rule specifications (without the
// -t filter -A/-C/-D prefix) that pin a TAP's bridge port to its assigned MAC
// and IP. ARP is allowed when the source MAC matches — without it the guest
// cannot resolve the gateway and all connectivity breaks; IPv4 is allowed only
// with the matching source MAC AND source IP; anything else from the port is
// dropped as spoofed. The trailing per-TAP DROP is scoped by -i, so other taps
// and host traffic are unaffected (the chain policy stays ACCEPT).
// 학습 주석: 세 규칙의 순서가 의미를 만든다 — ARP 허용, 정확한 (mac,ip) 조합의
// IPv4 만 허용, 그 외 이 tap 에서 나온 모든 트래픽 DROP. mac 만 맞고 ip 가 다르면
// (또는 그 반대) 세 번째 규칙에서 걸린다.
func antiSpoofRuleSets(tap, ip, mac string) [][]string {
	return [][]string{
		{"-p", "ARP", "-i", tap, "-s", mac, "-j", "ACCEPT"},
		{"-p", "IPv4", "-i", tap, "-s", mac, "--ip-src", ip, "-j", "ACCEPT"},
		{"-i", tap, "-j", "DROP"},
	}
}

// addAntiSpoofRules pins tap to (ip, mac) in the EPHEMERA_AS chain. No-op when
// anti-spoof is disabled. Each rule is added idempotently (-C check before -A).
// 학습 주석: VM spawn 경로에서 TAP 생성 직후 호출되는 것으로 추정되는 지점 —
// (tap, ip, mac) 세 값이 그 VM 에 daemon 이 실제로 할당한 값과 정확히 일치해야
// internal/mcpgateway 의 source-IP 신원 판정이 스푸핑에서 안전하다.
func (m *Manager) addAntiSpoofRules(tap, ip, mac string) {
	if !m.antiSpoof {
		return
	}
	for _, rule := range antiSpoofRuleSets(tap, ip, mac) {
		check := append([]string{"-t", "filter", "-C", antiSpoofChain}, rule...)
		if exec.Command("ebtables", check...).Run() != nil {
			add := append([]string{"-t", "filter", "-A", antiSpoofChain}, rule...)
			if err := exec.Command("ebtables", add...).Run(); err != nil {
				slog.Warn("anti-spoof rule add failed", "tap", tap, "err", err)
			}
		}
	}
	slog.Info("anti-spoof rules added", "tap", tap, "ip", ip, "mac", mac)
}

// removeAntiSpoofRules deletes tap's pinning rules from EPHEMERA_AS by exact
// match. No-op when anti-spoof is disabled. Exact-match removal (not a tap-name
// wildcard) is what keeps a recycled tap/IP from inheriting a stale pin.
// 학습 주석: VM 삭제 시 대응 정리 함수. tap 이름만으로 지우지 않고 (tap, ip, mac)
// 전체로 정확히 매치해야 지우는 이유가 바로 "재활용된 tap/IP 가 이전 VM 의 남은
// 규칙을 물려받지 않게" 하기 위함이다.
func (m *Manager) removeAntiSpoofRules(tap, ip, mac string) {
	if !m.antiSpoof {
		return
	}
	for _, rule := range antiSpoofRuleSets(tap, ip, mac) {
		del := append([]string{"-t", "filter", "-D", antiSpoofChain}, rule...)
		exec.Command("ebtables", del...).Run()
	}
}
