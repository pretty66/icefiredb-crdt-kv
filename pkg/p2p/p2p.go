package p2p

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/routing"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	discoveryRouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	"github.com/libp2p/go-libp2p/p2p/muxer/yamux"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	tls "github.com/libp2p/go-libp2p/p2p/security/tls"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	"github.com/mr-tron/base58/base58"
	"github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multihash"
	"github.com/sirupsen/logrus"
)

// P2P A structure that represents a P2P Host
type P2P struct {
	// Represents the host context layer
	Ctx context.Context

	// Represents the libp2p host
	Host host.Host

	// Represents the DHT routing table
	KadDHT *dht.IpfsDHT

	// Represents the peer discovery service
	Discovery *discoveryRouting.RoutingDiscovery

	// Represents the PubSub Handler
	PubSub *pubsub.PubSub

	service string
}

/*
A constructor function that generates and returns a P2P object.

Constructs a libp2p host with TLS encrypted secure transportation that works over a TCP
transport connection using a Yamux Stream Multiplexer and uses UPnP for the NAT traversal.

A Kademlia DHT is then bootstrapped on this host using the default peers offered by libp2p
and a Peer Discovery service is created from this Kademlia DHT. The PubSub handler is then
created on the host using the peer discovery service created prior.
*/
func NewP2P(serviceName string, priv crypto.PrivKey, port string) *P2P {
	// Setup a background context
	ctx := context.Background()

	// Setup a P2P Host Node :创建 p2p host
	nodehost, kaddht := setupHost(ctx, priv, port)
	// Debug log
	logrus.Debugln("Created the P2P Host and the Kademlia DHT.")

	// Bootstrap the Kad DHT :根据DHT启动节点
	bootstrapDHT(ctx, nodehost, kaddht)

	// Debug log
	logrus.Debugln("Bootstrapped the Kademlia DHT and Connected to Bootstrap Peers")

	// Create a peer discovery service using the Kad DHT : 创建一个节点路由发现方式
	routingdiscovery := discoveryRouting.NewRoutingDiscovery(kaddht)
	// Debug log
	logrus.Debugln("Created the Peer Discovery Service.")

	// Create a PubSub handler with the routing discovery：根据节点路由发现机制创建一个PubSub句柄
	pubsubhandler := setupPubSub(ctx, nodehost, routingdiscovery)
	// Debug log
	logrus.Debugln("Created the PubSub Handler.")

	// Return the P2P object
	return &P2P{
		Ctx:       ctx,
		Host:      nodehost,
		KadDHT:    kaddht,
		Discovery: routingdiscovery,
		PubSub:    pubsubhandler,
		service:   serviceName,
	}
}

// A method of P2P to connect to service peers.
// This method uses the Advertise() functionality of the Peer Discovery Service
// to advertise the service and then disovers all peers advertising the same.
// The peer discovery is handled by a go-routine that will read from a channel
// of peer address information until the peer channel closes
func (p2p *P2P) AdvertiseConnect() {
	// Advertise the availabilty of the service on this node
	ttl, err := p2p.Discovery.Advertise(p2p.Ctx, p2p.service)
	// Debug log
	logrus.Debugln("Advertised the PeerChat Service.")
	// Sleep to give time for the advertisment to propogate
	time.Sleep(time.Second * 5)
	// Debug log
	logrus.Debugf("Service Time-to-Live is %s", ttl)

	// Find all peers advertising the same service
	peerchan, err := p2p.Discovery.FindPeers(p2p.Ctx, p2p.service)
	// Handle any potential error
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Fatalln("P2P Peer Discovery Failed!")
	}
	// Trace log
	logrus.Traceln("Discovered PeerChat Service Peers.")

	// Connect to peers as they are discovered
	go handlePeerDiscovery(p2p.Host, peerchan)
	// Trace log
	logrus.Traceln("Started Peer Connection Handler.")
}

// A method of P2P to connect to service peers.
// This method uses the Provide() functionality of the Kademlia DHT directly to announce
// the ability to provide the service and then disovers all peers that provide the same.
// The peer discovery is handled by a go-routine that will read from a channel
// of peer address information until the peer channel closes
func (p2p *P2P) AnnounceConnect() {
	// Generate the Service CID
	cidvalue := generateCID(p2p.service)
	// Trace log
	logrus.Debug("cidvalue ", cidvalue.String())
	logrus.Traceln("Generated the Service CID.")

	// Announce that this host can provide the service CID
	err := p2p.KadDHT.Provide(p2p.Ctx, cidvalue, true)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Fatalln("Failed to Announce Service CID!")
	}
	// Debug log
	logrus.Debugln("Announced the PeerChat Service.")
	// Sleep to give time for the advertisment to propogate
	time.Sleep(time.Second * 5)

	// Find the other providers for the service CID
	peerchan := p2p.KadDHT.FindProvidersAsync(p2p.Ctx, cidvalue, 0)
	// Trace log
	logrus.Traceln("Discovered PeerChat Service Peers.")

	// Connect to peers as they are discovered
	go handlePeerDiscovery(p2p.Host, peerchan)
	// Debug log
	logrus.Debugln("Started Peer Connection Handler.")
}

// A function that generates the p2p configuration options and creates a
// libp2p host object for the given context. The created host is returned
func setupHost(ctx context.Context, prvkey crypto.PrivKey, port string) (host.Host, *dht.IpfsDHT) {
	// Set up the host identity options
	var err error
	if prvkey == nil {
		prvkey, _, err = crypto.GenerateKeyPairWithReader(crypto.RSA, 2048, rand.Reader)

		// Handle any potential error
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"error": err.Error(),
			}).Fatalln("Failed to Generate P2P Identity Configuration!")
		}
	}
	if len(port) == 0 {
		port = "0"
	}

	identity := libp2p.Identity(prvkey)

	// Trace log
	logrus.Traceln("Generated P2P Identity Configuration.")

	// Set up TLS secured TCP transport and options
	tlstransport, err := tls.New(prvkey)
	security := libp2p.Security(tls.ID, tlstransport)
	transport := libp2p.Transport(tcp.NewTCPTransport)
	// Handle any potential error
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Fatalln("Failed to Generate P2P Security and Transport Configurations!")
	}

	// Trace log
	logrus.Traceln("Generated P2P Security and Transport Configurations.")

	// Set up host listener address options
	muladdr, err := multiaddr.NewMultiaddr(fmt.Sprintf("/ip4/0.0.0.0/tcp/%s", port))
	listen := libp2p.ListenAddrs(muladdr)
	// Handle any potential error
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Fatalln("Failed to Generate P2P Address Listener Configuration!")
	}

	// Trace log
	logrus.Traceln("Generated P2P Address Listener Configuration.")
	// Set up the stream multiplexer and connection manager options
	muxer := libp2p.Muxer("/yamux/1.0.0", yamux.DefaultTransport)
	basicConnMgr, err := connmgr.NewConnManager(100, 400, connmgr.WithGracePeriod(time.Minute))
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Fatalln("Failed to NewConnManager!")
	}
	conn := libp2p.ConnectionManager(basicConnMgr)
	// Trace log
	logrus.Traceln("Generated P2P Stream Multiplexer, Connection Manager Configurations.")

	// Setup NAT traversal and relay options
	nat := libp2p.NATPortMap()
	relay := libp2p.EnableAutoRelay()

	// Trace log
	logrus.Traceln("Generated P2P NAT Traversal and Relay Configurations.")

	// Declare a KadDHT
	var kaddht *dht.IpfsDHT
	// Setup a routing configuration with the KadDHT
	//定义节点路由函数,设置节点发现函数
	routingOpt := libp2p.Routing(func(h host.Host) (routing.PeerRouting, error) {
		kaddht = setupKadDHT(ctx, h)
		return kaddht, err
	})

	// Trace log
	logrus.Traceln("Generated P2P Routing Configurations.")

	opts := libp2p.ChainOptions(identity, listen, security, transport, muxer, conn, nat, routingOpt, relay)

	// Construct a new libP2P host with the created options
	libhost, err := libp2p.New(opts)
	// Handle any potential error
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Fatalln("Failed to Create the P2P Host!")
	}

	// Return the created host and the kademlia DHT
	return libhost, kaddht
}

// A function that generates a Kademlia DHT object and returns it
func setupKadDHT(ctx context.Context, nodehost host.Host) *dht.IpfsDHT {
	// Create DHT server mode option
	dhtmode := dht.Mode(dht.ModeServer)
	// Rertieve the list of boostrap peer addresses
	bootstrappeers := dht.GetDefaultBootstrapPeerAddrInfos()
	// Create the DHT bootstrap peers option
	dhtpeers := dht.BootstrapPeers(bootstrappeers...)

	// Trace log
	logrus.Traceln("Generated DHT Configuration.")

	// Start a Kademlia DHT on the host in server mode
	kaddht, err := dht.New(ctx, nodehost, dhtmode, dhtpeers)
	// Handle any potential error
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Fatalln("Failed to Create the Kademlia DHT!")
	}

	// Return the KadDHT
	return kaddht
}

// A function that generates a PubSub Handler object and returns it
// Requires a node host and a routing discovery service.
func setupPubSub(ctx context.Context, nodehost host.Host, routingdiscovery *discoveryRouting.RoutingDiscovery) *pubsub.PubSub {
	// Create a new PubSub service which uses a GossipSub router
	pubsubhandler, err := pubsub.NewGossipSub(ctx, nodehost, pubsub.WithDiscovery(routingdiscovery))
	// Handle any potential error
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err.Error(),
			"type":  "GossipSub",
		}).Fatalln("PubSub Handler Creation Failed!")
	}

	// Return the PubSub handler
	return pubsubhandler
}

// A function that bootstraps a given Kademlia DHT to satisfy the IPFS router
// interface and connects to all the bootstrap peers provided by libp2p
func bootstrapDHT(ctx context.Context, nodehost host.Host, kaddht *dht.IpfsDHT) {
	// Bootstrap the DHT to satisfy the IPFS Router interface
	if err := kaddht.Bootstrap(ctx); err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Fatalln("Failed to Bootstrap the Kademlia!")
	}

	// Trace log
	logrus.Traceln("Set the Kademlia DHT into Bootstrap Mode.")

	// Declare a WaitGroup
	var wg sync.WaitGroup
	// Declare counters for the number of bootstrap peers
	var connectedbootpeers int32
	var totalbootpeers int32

	// Iterate over the default bootstrap peers provided by libp2p
	for _, peeraddr := range dht.DefaultBootstrapPeers {
		// Retrieve the peer address information
		peerinfo, _ := peer.AddrInfoFromP2pAddr(peeraddr)

		// Incremenent waitgroup counter
		wg.Add(1)
		totalbootpeers++
		// Start a goroutine to connect to each bootstrap peer
		go func() {
			// Defer the waitgroup decrement
			defer wg.Done()
			// Attempt to connect to the bootstrap peer
			if err := nodehost.Connect(ctx, *peerinfo); err == nil {
				// Increment the connected bootstrap peer count
				atomic.AddInt32(&connectedbootpeers, 1)
			}
		}()
	}

	// Wait for the waitgroup to complete
	wg.Wait()

	// Log the number of bootstrap peers connected
	logrus.Debugf("Connected to %d out of %d Bootstrap Peers.", connectedbootpeers, totalbootpeers)
}

// A function that connects the given host to all peers recieved from a
// channel of peer address information. Meant to be started as a go routine.
func handlePeerDiscovery(nodehost host.Host, peerchan <-chan peer.AddrInfo) {
	// Iterate over the peer channel
	for pe := range peerchan {
		// Ignore if the discovered peer is the host itself
		if pe.ID == nodehost.ID() {
			continue
		}
		if len(pe.Addrs) == 0 {
			continue
		}
		go func(p peer.AddrInfo) {
			// Connect to the peer
			err := nodehost.Connect(context.TODO(), p)
			if err != nil {
				logrus.Errorf("p2p peer %s connection failed: %v", p.String(), err)
			} else {
				logrus.Debugf("p2p peer connection success: %s", p.String())
				nodehost.ConnManager().TagPeer(p.ID, "keep", 100)
			}

		}(pe)
	}
}

// A function that generates a CID object for a given string and returns it.
// Uses SHA256 to hash the string and generate a multihash from it.
// The mulithash is then base58 encoded and then used to create the CID
func generateCID(namestring string) cid.Cid {
	// Hash the service content ID with SHA256
	hash := sha256.Sum256([]byte(namestring))
	// Append the hash with the hashing codec ID for SHA2-256 (0x12),
	// the digest size (0x20) and the hash of the service content ID
	finalhash := append([]byte{0x12, 0x20}, hash[:]...)
	// Encode the fullhash to Base58
	b58string := base58.Encode(finalhash)

	// Generate a Multihash from the base58 string
	mulhash, err := multihash.FromB58String(string(b58string))
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Fatalln("Failed to Generate Service CID!")
	}

	// Generate a CID from the Multihash
	cidvalue := cid.NewCidV1(12, mulhash)
	// Return the CID
	return cidvalue
}
