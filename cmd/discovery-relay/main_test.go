package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

func TestBroadcastTargets(test *testing.T) {
	targets, err := broadcastTargets("", []net.Addr{
		&net.IPNet{IP: net.ParseIP("10.23.4.10"), Mask: net.CIDRMask(23, 32)},
		&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
		&net.IPNet{IP: net.ParseIP("fd00::1"), Mask: net.CIDRMask(64, 128)},
	})
	if err != nil {
		test.Fatal(err)
	}
	found := make(map[string]bool)
	for _, target := range targets {
		if target.Port != 2000 || !target.IP.IsPrivate() {
			test.Fatalf("unsafe target: %v", target)
		}
		found[target.IP.String()] = true
	}
	for _, expected := range []string{"10.0.0.255", "192.168.1.255", "10.23.5.255"} {
		if !found[expected] {
			test.Errorf("missing default or interface broadcast %s", expected)
		}
	}
	targets, err = broadcastTargets("192.168.20.255, 192.168.20.255", nil)
	if err != nil || len(targets) != 1 || targets[0].IP.String() != "192.168.20.255" {
		test.Fatalf("override must replace defaults and deduplicate: %v, %v", targets, err)
	}
	for _, invalid := range []string{"1.1.1.1", "camera.local", "127.0.0.1", "fd00::1", "192.168.1.255,"} {
		if _, err := broadcastTargets(invalid, nil); err == nil {
			test.Errorf("accepted invalid target %q", invalid)
		}
	}
	var excessive []string
	for index := range 33 {
		excessive = append(excessive, fmt.Sprintf("10.1.%d.255", index))
	}
	if _, err := broadcastTargets(strings.Join(excessive, ","), nil); err == nil {
		test.Fatal("accepted more than 32 targets")
	}
}

func TestProbeFilterAndRateLimit(test *testing.T) {
	now := time.Unix(1000, 0)
	source := &net.UDPAddr{IP: net.ParseIP("172.21.0.2"), Port: 3000}
	if !acceptsProbe(discoveryProbe, source, now, time.Time{}) {
		test.Fatal("rejected the initial native probe")
	}
	if acceptsProbe(discoveryProbe, source, now, now.Add(-time.Millisecond)) {
		test.Fatal("accepted a repeated probe inside the rate limit")
	}
	if !acceptsProbe(discoveryProbe, source, now, now.Add(-time.Second)) {
		test.Fatal("rejected a probe after the rate limit")
	}
	for _, probe := range [][]byte{nil, {170, 170, 0}, {170, 170, 0, 0, 1}, {1, 2, 3, 4}} {
		if acceptsProbe(probe, source, now, time.Time{}) {
			test.Errorf("accepted non-discovery payload %v", probe)
		}
	}
	for _, invalid := range []*net.UDPAddr{nil, {IP: net.ParseIP("172.21.0.2"), Port: 2000}, {IP: net.ParseIP("1.1.1.1"), Port: 3000}} {
		if acceptsProbe(discoveryProbe, invalid, now, time.Time{}) {
			test.Errorf("accepted invalid source %v", invalid)
		}
	}
}

func TestRelayForwardsNativeProbeAndStops(test *testing.T) {
	receiver, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		test.Fatal(err)
	}
	defer receiver.Close()
	connection, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		test.Fatal(err)
	}
	defer connection.Close()
	sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 3000})
	if err != nil {
		test.Fatal(err)
	}
	defer sender.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		finished <- relay(ctx, connection, []*net.UDPAddr{receiver.LocalAddr().(*net.UDPAddr)}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	}()
	if _, err := sender.WriteToUDP(discoveryProbe, connection.LocalAddr().(*net.UDPAddr)); err != nil {
		test.Fatal(err)
	}
	if err := receiver.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		test.Fatal(err)
	}
	var buffer [16]byte
	count, _, err := receiver.ReadFromUDP(buffer[:])
	if err != nil || !bytes.Equal(buffer[:count], discoveryProbe) {
		test.Fatalf("native probe was not forwarded: %v", err)
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			test.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		test.Fatal("relay did not stop after cancellation")
	}
}
