package network

import (
	"errors"
	"reflect"
	"testing"
)

func TestSetupBridgeAddsForwardAcceptRules(t *testing.T) {
	m := &Manager{
		gatewayIP:  "10.0.1.1",
		subnet:     "10.0.1.",
		bridgeName: "goose-br0",
	}

	var commands [][]string
	m.runCommand = func(name string, args ...string) error {
		command := append([]string{name}, args...)
		commands = append(commands, command)
		if name == "iptables" && len(args) >= 3 && args[0] == "-C" {
			return errors.New("missing rule")
		}
		if name == "iptables" && len(args) >= 5 && args[0] == "-t" && args[1] == "nat" && args[2] == "-C" {
			return errors.New("missing rule")
		}
		return nil
	}

	if err := m.setupBridge(); err != nil {
		t.Fatalf("setupBridge: %v", err)
	}

	wantOutbound := []string{"iptables", "-I", "FORWARD", "-i", "goose-br0", "-s", "10.0.1.0/24", "-j", "ACCEPT"}
	wantReturn := []string{"iptables", "-I", "FORWARD", "-o", "goose-br0", "-d", "10.0.1.0/24", "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}
	for _, want := range [][]string{wantOutbound, wantReturn} {
		if !hasCommand(commands, want) {
			t.Fatalf("missing command %v in %v", want, commands)
		}
	}
}

func TestAllowBridgeHostPortAddsInputAcceptRule(t *testing.T) {
	m := &Manager{
		gatewayIP:  "10.0.1.1",
		subnet:     "10.0.1.",
		bridgeName: "goose-br0",
	}

	var commands [][]string
	m.runCommand = func(name string, args ...string) error {
		command := append([]string{name}, args...)
		commands = append(commands, command)
		if name == "iptables" && len(args) >= 3 && args[0] == "-C" {
			return errors.New("missing rule")
		}
		return nil
	}

	if err := m.AllowBridgeHostPort(3000); err != nil {
		t.Fatalf("AllowBridgeHostPort: %v", err)
	}

	wantCheck := []string{"iptables", "-C", "INPUT", "-i", "goose-br0", "-s", "10.0.1.0/24", "-d", "10.0.1.1", "-p", "tcp", "--dport", "3000", "-j", "ACCEPT", "-m", "comment", "--comment", "anvil-cp-callback"}
	wantInsert := []string{"iptables", "-I", "INPUT", "-i", "goose-br0", "-s", "10.0.1.0/24", "-d", "10.0.1.1", "-p", "tcp", "--dport", "3000", "-j", "ACCEPT", "-m", "comment", "--comment", "anvil-cp-callback"}
	for _, want := range [][]string{wantCheck, wantInsert} {
		if !hasCommand(commands, want) {
			t.Fatalf("missing command %v in %v", want, commands)
		}
	}
}

func TestAllowBridgeHostPortDoesNotDuplicateExistingRule(t *testing.T) {
	m := &Manager{
		gatewayIP:  "10.0.1.1",
		subnet:     "10.0.1.",
		bridgeName: "goose-br0",
	}

	var commands [][]string
	m.runCommand = func(name string, args ...string) error {
		commands = append(commands, append([]string{name}, args...))
		return nil
	}

	if err := m.AllowBridgeHostPort(3000); err != nil {
		t.Fatalf("AllowBridgeHostPort: %v", err)
	}

	wantInsert := []string{"iptables", "-I", "INPUT", "-i", "goose-br0", "-s", "10.0.1.0/24", "-d", "10.0.1.1", "-p", "tcp", "--dport", "3000", "-j", "ACCEPT", "-m", "comment", "--comment", "anvil-cp-callback"}
	if hasCommand(commands, wantInsert) {
		t.Fatalf("unexpected duplicate insert command in %v", commands)
	}
}

func hasCommand(commands [][]string, want []string) bool {
	for _, got := range commands {
		if reflect.DeepEqual(got, want) {
			return true
		}
	}
	return false
}
