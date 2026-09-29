package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
)

func TestSourceIPsForAddress(t *testing.T) {
	for _, test := range []struct {
		name string
		addr string
		want string
	}{
		{"127.0.0.1", "localhost:80", "127.0.0.1"},
		{"::1", "[::1]:80", "::1"},
	} {
		ips, err := sourceIPsForAddress(test.name, test.addr)
		if err != nil || len(ips) != 1 || ips[0].String() != test.want {
			t.Fatalf("sourceIPsForAddress(%q, %q) = %v, %v", test.name, test.addr, ips, err)
		}
	}
	if _, err := sourceIPsForAddress("127.0.0.1", "[::1]:80"); err == nil {
		t.Fatal("expected address family mismatch")
	}
	if _, err := sourceIPs("no-such-transfer-interface"); err == nil {
		t.Fatal("expected unknown interface error")
	}
	if _, err := sourceIPs("192.0.2.123"); err == nil {
		t.Fatal("expected non-local IP address error")
	}
}

func TestSourceIPsAcceptsInterfaceName(t *testing.T) {
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || !ipNet.IP.Equal(net.IPv4(127, 0, 0, 1)) {
				continue
			}
			ips, err := sourceIPsForAddress(iface.Name, "127.0.0.1:80")
			if err != nil {
				t.Fatal(err)
			}
			for _, ip := range ips {
				if ip.Equal(net.IPv4(127, 0, 0, 1)) {
					return
				}
			}
			t.Fatalf("interface %q did not resolve to loopback: %v", iface.Name, ips)
		}
	}
	t.Fatal("no IPv4 loopback interface found")
}

func TestDialOutboundUsesSelectedSourceIP(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	previous := interfaceName
	interfaceName = "127.0.0.1"
	defer func() { interfaceName = previous }()

	conn, err := dialOutbound(context.Background(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := conn.LocalAddr().(*net.TCPAddr).IP.String(); got != interfaceName {
		t.Fatalf("local IP = %s, want %s", got, interfaceName)
	}
}

func TestHTTP3UsesSelectedSourceIP(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	server := &http3.Server{
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("ok"))
		}),
	}
	go server.Serve(listener)
	defer server.Close()

	previousName, previousVerify := interfaceName, insecureSkipVerify
	interfaceName, insecureSkipVerify = "127.0.0.1", true
	defer func() { interfaceName, insecureSkipVerify = previousName, previousVerify }()
	client := getHTTPClient(true)
	defer client.Transport.(*http3.RoundTripper).Close()
	resp, err := client.Get("https://" + listener.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "ok" {
		t.Fatalf("HTTP/3 response = %q, %v", body, err)
	}
}
