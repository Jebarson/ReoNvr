package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
)

const defaultBroadcasts = "10.0.0.255,10.0.1.255,192.168.0.255,192.168.1.255,192.168.4.255,192.168.8.255,192.168.31.255,192.168.50.255,192.168.68.255,192.168.86.255,192.168.100.255,192.168.178.255,192.168.254.255"

var discoveryProbe = []byte{170, 170, 0, 0}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		logger.Warn("interface discovery unavailable; using configured or common broadcasts")
	}
	targets, err := broadcastTargets(os.Getenv("REONVR_DISCOVERY_BROADCASTS"), addresses)
	if err == nil {
		var connection *net.UDPConn
		connection, err = net.ListenUDP("udp4", &net.UDPAddr{Port: 2000})
		if err == nil {
			defer connection.Close()
			logger.Info("discovery relay ready", "targets", len(targets))
			err = relay(ctx, connection, targets, logger)
		}
	}
	if err != nil {
		logger.Error("discovery relay stopped", "error", err)
		os.Exit(1)
	}
}

func broadcastTargets(configured string, addresses []net.Addr) ([]*net.UDPAddr, error) {
	values := strings.Split(defaultBroadcasts, ",")
	if strings.TrimSpace(configured) != "" {
		if len(configured) > 4096 {
			return nil, errors.New("discovery broadcast configuration is too long")
		}
		values = strings.Split(configured, ",")
	} else {
		for _, address := range addresses {
			addressIP, network, err := net.ParseCIDR(address.String())
			if err != nil || addressIP.To4() == nil || !addressIP.IsPrivate() {
				continue
			}
			prefix, bits := network.Mask.Size()
			if bits != 32 || prefix > 30 {
				continue
			}
			broadcast := make(net.IP, net.IPv4len)
			for index := range broadcast {
				broadcast[index] = addressIP.To4()[index] | ^network.Mask[index]
			}
			if broadcast.IsPrivate() && len(values) < 32 {
				values = append(values, broadcast.String())
			}
		}
	}
	unique := make(map[string]*net.UDPAddr)
	for _, value := range values {
		address := net.ParseIP(strings.TrimSpace(value))
		if address == nil || address.To4() == nil || (!address.IsPrivate() && !address.IsLinkLocalUnicast()) {
			return nil, errors.New("discovery broadcasts must be private or link-local IPv4 addresses")
		}
		unique[address.String()] = &net.UDPAddr{IP: address.To4(), Port: 2000}
		if len(unique) > 32 {
			return nil, errors.New("at most 32 discovery broadcasts are allowed")
		}
	}
	keys := make([]string, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	targets := make([]*net.UDPAddr, 0, len(keys))
	for _, key := range keys {
		targets = append(targets, unique[key])
	}
	return targets, nil
}

func acceptsProbe(probe []byte, source *net.UDPAddr, now, lastForward time.Time) bool {
	return source != nil && source.Port == 3000 &&
		(source.IP.IsPrivate() || source.IP.IsLoopback() || source.IP.IsLinkLocalUnicast()) &&
		bytes.Equal(probe, discoveryProbe) && (lastForward.IsZero() || now.Sub(lastForward) >= time.Second)
}

func relay(ctx context.Context, connection *net.UDPConn, targets []*net.UDPAddr, logger *slog.Logger) error {
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	var buffer [5]byte
	var lastForward time.Time
	for {
		count, source, err := connection.ReadFromUDP(buffer[:])
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		now := time.Now()
		if !acceptsProbe(buffer[:count], source, now, lastForward) {
			continue
		}
		lastForward = now
		if err := connection.SetWriteDeadline(now.Add(time.Second)); err != nil {
			return err
		}
		failed := 0
		for _, target := range targets {
			if _, err := connection.WriteToUDP(discoveryProbe, target); err != nil {
				failed++
			}
		}
		logger.Info("discovery probe forwarded", "targets", len(targets), "failedTargets", failed)
	}
}
