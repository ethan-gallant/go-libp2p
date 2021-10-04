package test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"

	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/mux"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"

	logging "github.com/ipfs/go-log/v2"
	bhost "github.com/libp2p/go-libp2p-blankhost"
	"github.com/libp2p/go-libp2p-core/metrics"
	"github.com/libp2p/go-libp2p-peerstore/pstoremem"
	swarm "github.com/libp2p/go-libp2p-swarm"
	swarmt "github.com/libp2p/go-libp2p-swarm/testing"
	tptu "github.com/libp2p/go-libp2p-transport-upgrader"
	"github.com/libp2p/go-tcp-transport"
	ma "github.com/multiformats/go-multiaddr"
)

func init() {
	// TODO temporary for debugging purposes; to be removed for merge.
	logging.SetLogLevel("relay", "DEBUG")
	logging.SetLogLevel("p2p-circuit", "DEBUG")
}

func getNetHosts(t *testing.T, ctx context.Context, n int) (hosts []host.Host, upgraders []*tptu.Upgrader) {
	for i := 0; i < n; i++ {
		privk, pubk, err := crypto.GenerateKeyPair(crypto.Ed25519, 0)
		if err != nil {
			t.Fatal(err)
		}

		p, err := peer.IDFromPublicKey(pubk)
		if err != nil {
			t.Fatal(err)
		}

		ps := pstoremem.NewPeerstore()
		err = ps.AddPrivKey(p, privk)
		if err != nil {
			t.Fatal(err)
		}

		bwr := metrics.NewBandwidthCounter()
		netw, err := swarm.NewSwarm(p, ps, swarm.WithMetrics(bwr))
		if err != nil {
			t.Fatal(err)
		}

		upgrader := swarmt.GenUpgrader(netw)
		upgraders = append(upgraders, upgrader)

		err = netw.AddTransport(tcp.NewTCPTransport(upgrader))
		if err != nil {
			t.Fatal(err)
		}

		err = netw.Listen(ma.StringCast("/ip4/127.0.0.1/tcp/0"))
		if err != nil {
			t.Fatal(err)
		}

		h := bhost.NewBlankHost(netw)

		hosts = append(hosts, h)
	}

	return hosts, upgraders
}

func connect(t *testing.T, a, b host.Host) {
	pi := peer.AddrInfo{ID: a.ID(), Addrs: a.Addrs()}
	err := b.Connect(context.Background(), pi)
	if err != nil {
		t.Fatal(err)
	}
}

func addTransport(t *testing.T, h host.Host, upgrader *tptu.Upgrader) {
	if err := client.AddTransport(h, upgrader); err != nil {
		t.Fatal(err)
	}
}

func TestBasicRelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hosts, upgraders := getNetHosts(t, ctx, 3)
	addTransport(t, hosts[0], upgraders[0])
	addTransport(t, hosts[2], upgraders[2])

	rch := make(chan []byte, 1)
	hosts[0].SetStreamHandler("test", func(s network.Stream) {
		defer s.Close()
		defer close(rch)

		buf := make([]byte, 1024)
		nread := 0
		for nread < len(buf) {
			n, err := s.Read(buf[nread:])
			nread += n
			if err != nil {
				if err == io.EOF {
					break
				}
				t.Fatal(err)
			}
		}

		rch <- buf[:nread]
	})

	r, err := relay.New(hosts[1])
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	connect(t, hosts[0], hosts[1])
	connect(t, hosts[1], hosts[2])

	rinfo := hosts[1].Peerstore().PeerInfo(hosts[1].ID())
	rsvp, err := client.Reserve(ctx, hosts[0], rinfo)
	if err != nil {
		t.Fatal(err)
	}

	if rsvp.Voucher == nil {
		t.Fatal("no reservation voucher")
	}

	raddr, err := ma.NewMultiaddr(fmt.Sprintf("/p2p/%s/p2p-circuit/p2p/%s", hosts[1].ID(), hosts[0].ID()))
	if err != nil {
		t.Fatal(err)
	}

	err = hosts[2].Connect(ctx, peer.AddrInfo{ID: hosts[0].ID(), Addrs: []ma.Multiaddr{raddr}})
	if err != nil {
		t.Fatal(err)
	}

	conns := hosts[2].Network().ConnsToPeer(hosts[0].ID())
	if len(conns) != 1 {
		t.Fatalf("expected 1 connection, but got %d", len(conns))
	}
	if !conns[0].Stat().Transient {
		t.Fatal("expected transient connection")
	}

	s, err := hosts[2].NewStream(network.WithUseTransient(ctx, "test"), hosts[0].ID(), "test")
	if err != nil {
		t.Fatal(err)
	}

	msg := []byte("relay works!")
	nwritten, err := s.Write(msg)
	if err != nil {
		t.Fatal(err)
	}
	if nwritten != len(msg) {
		t.Fatalf("expected to write %d bytes, but wrote %d instead", len(msg), nwritten)
	}
	s.CloseWrite()

	got := <-rch
	if !bytes.Equal(msg, got) {
		t.Fatalf("Wrong echo; expected %s but got %s", string(msg), string(got))
	}
}

func TestRelayLimitTime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hosts, upgraders := getNetHosts(t, ctx, 3)
	addTransport(t, hosts[0], upgraders[0])
	addTransport(t, hosts[2], upgraders[2])

	rch := make(chan error, 1)
	hosts[0].SetStreamHandler("test", func(s network.Stream) {
		defer s.Close()
		defer close(rch)

		buf := make([]byte, 1024)
		_, err := s.Read(buf)
		rch <- err
	})

	rc := relay.DefaultResources()
	rc.Limit.Duration = time.Second

	r, err := relay.New(hosts[1], relay.WithResources(rc))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	connect(t, hosts[0], hosts[1])
	connect(t, hosts[1], hosts[2])

	rinfo := hosts[1].Peerstore().PeerInfo(hosts[1].ID())
	_, err = client.Reserve(ctx, hosts[0], rinfo)
	if err != nil {
		t.Fatal(err)
	}

	raddr, err := ma.NewMultiaddr(fmt.Sprintf("/p2p/%s/p2p-circuit/p2p/%s", hosts[1].ID(), hosts[0].ID()))
	if err != nil {
		t.Fatal(err)
	}

	err = hosts[2].Connect(ctx, peer.AddrInfo{ID: hosts[0].ID(), Addrs: []ma.Multiaddr{raddr}})
	if err != nil {
		t.Fatal(err)
	}

	conns := hosts[2].Network().ConnsToPeer(hosts[0].ID())
	if len(conns) != 1 {
		t.Fatalf("expected 1 connection, but got %d", len(conns))
	}
	if !conns[0].Stat().Transient {
		t.Fatal("expected transient connection")
	}

	s, err := hosts[2].NewStream(network.WithUseTransient(ctx, "test"), hosts[0].ID(), "test")
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(2 * time.Second)
	n, err := s.Write([]byte("should be closed"))
	if n > 0 {
		t.Fatalf("expected to write 0 bytes, wrote %d", n)
	}
	if err != mux.ErrReset {
		t.Fatalf("expected reset, but got %s", err)
	}

	err = <-rch
	if err != mux.ErrReset {
		t.Fatalf("expected reset, but got %s", err)
	}
}

func TestRelayLimitData(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hosts, upgraders := getNetHosts(t, ctx, 3)
	addTransport(t, hosts[0], upgraders[0])
	addTransport(t, hosts[2], upgraders[2])

	rch := make(chan int, 1)
	hosts[0].SetStreamHandler("test", func(s network.Stream) {
		defer s.Close()
		defer close(rch)

		buf := make([]byte, 1024)
		for i := 0; i < 3; i++ {
			n, err := s.Read(buf)
			if err != nil {
				t.Fatal(err)
			}
			rch <- n
		}

		n, err := s.Read(buf)
		if err != mux.ErrReset {
			t.Fatalf("expected reset but got %s", err)
		}
		rch <- n
	})

	rc := relay.DefaultResources()
	rc.Limit.Duration = time.Second
	rc.Limit.Data = 4096

	r, err := relay.New(hosts[1], relay.WithResources(rc))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	connect(t, hosts[0], hosts[1])
	connect(t, hosts[1], hosts[2])

	rinfo := hosts[1].Peerstore().PeerInfo(hosts[1].ID())
	_, err = client.Reserve(ctx, hosts[0], rinfo)
	if err != nil {
		t.Fatal(err)
	}

	raddr, err := ma.NewMultiaddr(fmt.Sprintf("/p2p/%s/p2p-circuit/p2p/%s", hosts[1].ID(), hosts[0].ID()))
	if err != nil {
		t.Fatal(err)
	}

	err = hosts[2].Connect(ctx, peer.AddrInfo{ID: hosts[0].ID(), Addrs: []ma.Multiaddr{raddr}})
	if err != nil {
		t.Fatal(err)
	}

	conns := hosts[2].Network().ConnsToPeer(hosts[0].ID())
	if len(conns) != 1 {
		t.Fatalf("expected 1 connection, but got %d", len(conns))
	}
	if !conns[0].Stat().Transient {
		t.Fatal("expected transient connection")
	}

	s, err := hosts[2].NewStream(network.WithUseTransient(ctx, "test"), hosts[0].ID(), "test")
	if err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 1024)
	for i := 0; i < 3; i++ {
		_, err = rand.Read(buf)
		if err != nil {
			t.Fatal(err)
		}

		n, err := s.Write(buf)
		if err != nil {
			t.Fatal(err)
		}
		if n != len(buf) {
			t.Fatalf("expected to write %d bytes but wrote %d", len(buf), n)
		}

		n = <-rch
		if n != len(buf) {
			t.Fatalf("expected to read %d bytes but read %d", len(buf), n)
		}
	}

	buf = make([]byte, 4096)
	_, err = rand.Read(buf)
	if err != nil {
		t.Fatal(err)
	}

	s.Write(buf)

	n := <-rch
	if n != 0 {
		t.Fatalf("expected to read 0 bytes but read %d", n)
	}

}