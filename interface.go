package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/quic-go/quic-go"
)

func sourceIPs(name string) ([]net.IP, error) {
	if name == "" {
		return nil, nil
	}
	if ip := net.ParseIP(name); ip != nil {
		addrs, err := net.InterfaceAddrs()
		if err != nil {
			return nil, fmt.Errorf("local interface addresses: %w", err)
		}
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.Equal(ip) {
				return []net.IP{ip}, nil
			}
		}
		return nil, fmt.Errorf("IP address %q is not assigned to a local interface", name)
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, fmt.Errorf("interface %q: %w", name, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, fmt.Errorf("interface %q addresses: %w", name, err)
	}
	var ips []net.IP
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsUnspecified() && !ipNet.IP.IsLinkLocalUnicast() {
			ips = append(ips, ipNet.IP)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("interface %q has no usable IP address", name)
	}
	return ips, nil
}

func sourceIPsForAddress(name, addr string) ([]net.IP, error) {
	ips, err := sourceIPs(name)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	remoteIP := net.ParseIP(host)
	var selected []net.IP
	for _, ip := range ips {
		if remoteIP == nil || (ip.To4() == nil) == (remoteIP.To4() == nil) {
			selected = append(selected, ip)
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("interface %q has no address matching %s", name, addr)
	}
	return selected, nil
}

func dialOutbound(ctx context.Context, network, addr string) (net.Conn, error) {
	ips, err := sourceIPsForAddress(interfaceName, addr)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		ips = []net.IP{nil}
	}
	var dialErr error
	for _, ip := range ips {
		dialer := &net.Dialer{Timeout: 30 * time.Second}
		if ip != nil {
			dialer.LocalAddr = &net.TCPAddr{IP: ip}
		}
		conn, err := dialer.DialContext(ctx, network, addr)
		if err == nil {
			if tcpConn, ok := conn.(*net.TCPConn); ok {
				_ = tcpConn.SetKeepAlive(false)
			}
			return conn, nil
		}
		dialErr = errors.Join(dialErr, err)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, dialErr
}

func dialOutboundQUIC(ctx context.Context, addr string, tlsCfg *tls.Config, cfg *quic.Config) (quic.EarlyConnection, error) {
	ips, err := sourceIPsForAddress(interfaceName, addr)
	if err != nil {
		return nil, err
	}
	var dialErr error
	for _, ip := range ips {
		network := "udp6"
		if ip.To4() != nil {
			network = "udp4"
		}
		remote, err := net.ResolveUDPAddr(network, addr)
		if err != nil {
			dialErr = errors.Join(dialErr, err)
			continue
		}
		udpConn, err := net.ListenUDP(network, &net.UDPAddr{IP: ip})
		if err != nil {
			dialErr = errors.Join(dialErr, err)
			continue
		}
		conn, err := quic.DialEarly(ctx, udpConn, remote, tlsCfg, cfg)
		if err != nil {
			udpConn.Close()
			dialErr = errors.Join(dialErr, err)
			continue
		}
		go func() {
			<-conn.Context().Done()
			udpConn.Close()
		}()
		return conn, nil
	}
	return nil, dialErr
}
