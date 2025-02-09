package main

import (
	"context"
	crand "crypto/rand"
	"fmt"
	"net/http"
	"os"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	options "github.com/dgraph-io/badger/v4/options"
	bsclient "github.com/ipfs/boxo/bitswap/client"
	bsnet "github.com/ipfs/boxo/bitswap/network"
	"github.com/ipfs/boxo/blockservice"
	"github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/gateway"
	"github.com/ipfs/boxo/ipns"
	"github.com/ipfs/boxo/namesys"
	routingv1client "github.com/ipfs/boxo/routing/http/client"
	httpcontentrouter "github.com/ipfs/boxo/routing/http/contentrouter"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	badger4 "github.com/ipfs/go-ds-badger4"
	levelds "github.com/ipfs/go-ds-leveldb"
	metri "github.com/ipfs/go-metrics-interface"
	mprome "github.com/ipfs/go-metrics-prometheus"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p-kad-dht/fullrt"
	record "github.com/libp2p/go-libp2p-record"
	routinghelpers "github.com/libp2p/go-libp2p-routing-helpers"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/metrics"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/routing"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/multiformats/go-multiaddr"
	"go.opencensus.io/stats/view"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

func init() {
	if err := mprome.Inject(); err != nil {
		panic(err)
	}
}

const ipniFallbackEndpoint = "https://cid.contact"

type Node struct {
	vs   routing.ValueStore
	host host.Host

	datastore  datastore.Batching
	blockstore blockstore.Blockstore
	bsClient   *bsclient.Client
	bsrv       blockservice.BlockService

	ns       namesys.NameSystem
	kuboRPCs []string

	bwc *metrics.BandwidthCounter
}

type Config struct {
	ListenAddrs   []string
	AnnounceAddrs []string

	Libp2pKeyFile string

	ConnMgrLow   int
	ConnMgrHi    int
	ConnMgrGrace time.Duration

	InMemBlockCache int64

	RoutingV1     string
	KuboRPCURLs   []string
	DHTSharedHost bool
	DNSCache      *cachedDNS
}

func Setup(ctx context.Context, cfg Config) (*Node, error) {
	peerkey, err := loadOrInitPeerKey(cfg.Libp2pKeyFile)
	if err != nil {
		return nil, err
	}

	ds, err := setupDatastore(cfg)
	if err != nil {
		return nil, err
	}

	bwc := metrics.NewBandwidthCounter()

	cmgr, err := connmgr.NewConnManager(cfg.ConnMgrLow, cfg.ConnMgrHi, connmgr.WithGracePeriod(cfg.ConnMgrGrace))
	if err != nil {
		return nil, err
	}

	opts := []libp2p.Option{
		libp2p.ListenAddrStrings(cfg.ListenAddrs...),
		libp2p.NATPortMap(),
		libp2p.ConnectionManager(cmgr),
		libp2p.Identity(peerkey),
		libp2p.BandwidthReporter(bwc),
		libp2p.DefaultTransports,
		libp2p.DefaultMuxers,
	}

	if len(cfg.AnnounceAddrs) > 0 {
		var addrs []multiaddr.Multiaddr
		for _, anna := range cfg.AnnounceAddrs {
			a, err := multiaddr.NewMultiaddr(anna)
			if err != nil {
				return nil, fmt.Errorf("failed to parse announce addr: %w", err)
			}
			addrs = append(addrs, a)
		}
		opts = append(opts, libp2p.AddrsFactory(func([]multiaddr.Multiaddr) []multiaddr.Multiaddr {
			return addrs
		}))
	}

	blkst := blockstore.NewBlockstore(ds,
		blockstore.NoPrefix(),
		// Every Has() for every written block is a transaction with a
		// seek onto LSM. If not in memory it will be a pain.
		// We opt to write every block Put into the blockstore.
		// See also comment in blockservice.
		blockstore.WriteThrough(),
	)
	blkst = blockstore.NewIdStore(blkst)

	bsctx := metri.CtxScope(ctx, "rainbow")

	var pr routing.PeerRouting
	var vs routing.ValueStore
	var cr routing.ContentRouting

	// Increase per-host connection pool since we are making lots of concurrent requests.
	httpClient := &http.Client{
		Transport: otelhttp.NewTransport(
			&routingv1client.ResponseBodyLimitedTransport{
				RoundTripper: &customTransport{
					// Roundtripper with increased defaults than http.Transport such that retrieving
					// multiple lookups concurrently is fast.
					RoundTripper: &http.Transport{
						MaxIdleConns:        1000,
						MaxConnsPerHost:     100,
						MaxIdleConnsPerHost: 100,
						IdleConnTimeout:     90 * time.Second,
						DialContext:         cfg.DNSCache.dialWithCachedDNS,
						ForceAttemptHTTP2:   true,
					},
				},
				LimitBytes: 1 << 20,
			}),
	}

	opts = append(opts, libp2p.Routing(func(h host.Host) (routing.PeerRouting, error) {
		if cfg.RoutingV1 != "" {
			routingClient, err := delegatedHTTPContentRouter(cfg.RoutingV1, routingv1client.WithStreamResultsRequired(), routingv1client.WithHTTPClient(httpClient))
			if err != nil {
				return nil, err
			}
			pr = routingClient
			vs = routingClient
			cr = routingClient
		} else {
			// If there are no delegated routing endpoints run an accelerated Amino DHT client and send IPNI requests to cid.contact

			// TODO: This datastore shouldn't end up containing anything anyway so this could potentially just be a null datastore
			memDS, err := levelds.NewDatastore("", nil)
			if err != nil {
				return nil, err
			}

			var dhtHost host.Host
			if cfg.DHTSharedHost {
				dhtHost = h
			} else {
				dhtHost, err = libp2p.New(
					libp2p.NoListenAddrs,
					libp2p.BandwidthReporter(bwc),
					libp2p.DefaultTransports,
					libp2p.DefaultMuxers,
				)
				if err != nil {
					return nil, err
				}
			}

			standardClient, err := dht.New(ctx, dhtHost,
				dht.Datastore(memDS),
				dht.BootstrapPeers(dht.GetDefaultBootstrapPeerAddrInfos()...),
				dht.Mode(dht.ModeClient),
			)
			if err != nil {
				return nil, err
			}

			fullRTClient, err := fullrt.NewFullRT(dhtHost, dht.DefaultPrefix,
				fullrt.DHTOption(
					dht.Validator(record.NamespacedValidator{
						"pk":   record.PublicKeyValidator{},
						"ipns": ipns.Validator{KeyBook: h.Peerstore()},
					}),
					dht.Datastore(memDS),
					dht.BootstrapPeers(dht.GetDefaultBootstrapPeerAddrInfos()...),
					dht.BucketSize(20),
				))
			if err != nil {
				return nil, err
			}

			dhtRouter := &bundledDHT{
				standard: standardClient,
				fullRT:   fullRTClient,
			}

			// we want to also use the default HTTP routers, so wrap the FullRT client
			// in a parallel router that calls them in parallel
			httpRouters, err := delegatedHTTPContentRouter(ipniFallbackEndpoint, routingv1client.WithHTTPClient(httpClient))
			if err != nil {
				return nil, err
			}
			routers := []*routinghelpers.ParallelRouter{
				{
					Router:                  dhtRouter,
					ExecuteAfter:            0,
					DoNotWaitForSearchValue: true,
					IgnoreError:             false,
				},
				{
					Timeout:                 15 * time.Second,
					Router:                  httpRouters,
					ExecuteAfter:            0,
					DoNotWaitForSearchValue: true,
					IgnoreError:             true,
				},
			}
			router := routinghelpers.NewComposableParallel(routers)

			pr = router
			vs = router
			cr = router
		}

		return pr, nil
	}))
	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, err
	}

	bn := bsnet.NewFromIpfsHost(h, cr)
	bswap := bsclient.New(bsctx, bn, blkst)
	bn.Start(bswap)

	bsrv := blockservice.New(blkst, bswap,
		// if we are doing things right, our bitswap wantlists should
		// not have blocks that we already have (see
		// https://github.com/ipfs/boxo/blob/e0d4b3e9b91e9904066a10278e366c9a6d9645c7/blockservice/blockservice.go#L272). Thus
		// we should not be writing many blocks that we already
		// have. Thus, no point in checking whether we have a block
		// before writing new blocks.
		blockservice.WriteThrough(),
	)

	dns, err := gateway.NewDNSResolver(nil)
	if err != nil {
		return nil, err
	}
	ns, err := namesys.NewNameSystem(vs, namesys.WithDNSResolver(dns))
	if err != nil {
		return nil, err
	}

	return &Node{
		host:       h,
		blockstore: blkst,
		datastore:  ds,
		bsClient:   bswap,
		ns:         ns,
		vs:         vs,
		bsrv:       bsrv,
		bwc:        bwc,
		kuboRPCs:   cfg.KuboRPCURLs,
	}, nil
}

func setupDatastore(cfg Config) (datastore.Batching, error) {
	badgerOpts := badger.DefaultOptions("")
	badgerOpts.CompactL0OnClose = false
	// ValueThreshold: defaults to 1MB! For us that means everything goes
	// into the LSM tree and that means more stuff in memory.  We only
	// put very small things on the LSM tree by default (i.e. a single
	// CID).
	badgerOpts.ValueThreshold = 256

	// BlockCacheSize: instead of using blockstore, we cache things
	// here. This only makes sense if using compression, according to
	// docs.
	badgerOpts.BlockCacheSize = cfg.InMemBlockCache // default 1 GiB.

	// Compression: default. Trades reading less from disk for using more
	// CPU. Given gateways are usually IO bound, I think we can make this
	// trade.
	if badgerOpts.BlockCacheSize == 0 {
		badgerOpts.Compression = options.None
	} else {
		badgerOpts.Compression = options.Snappy
	}

	// If we write something twice, we do it with the same values so
	// *shrugh*.
	badgerOpts.DetectConflicts = false

	// MemTableSize: Defaults to 64MiB which seems an ok amount to flush
	// to disk from time to time.
	badgerOpts.MemTableSize = 64 << 20
	// NumMemtables: more means more memory, faster writes, but more to
	// commit to disk if they get full. Default is 5.
	badgerOpts.NumMemtables = 5

	// IndexCacheSize: 0 means all in memory (default). All means indexes,
	// bloom filters etc. Usually not huge amount of memory usage from
	// this.
	badgerOpts.IndexCacheSize = 0

	opts := badger4.Options{
		GcDiscardRatio: 0.3,
		GcInterval:     20 * time.Minute,
		GcSleep:        10 * time.Second,
		Options:        badgerOpts,
	}

	return badger4.NewDatastore("badger4", &opts)
}

type bundledDHT struct {
	standard *dht.IpfsDHT
	fullRT   *fullrt.FullRT
}

func (b *bundledDHT) getDHT() routing.Routing {
	if b.fullRT.Ready() {
		return b.fullRT
	}
	return b.standard
}

func (b *bundledDHT) Provide(ctx context.Context, c cid.Cid, brdcst bool) error {
	return b.getDHT().Provide(ctx, c, brdcst)
}

func (b *bundledDHT) FindProvidersAsync(ctx context.Context, c cid.Cid, i int) <-chan peer.AddrInfo {
	return b.getDHT().FindProvidersAsync(ctx, c, i)
}

func (b *bundledDHT) FindPeer(ctx context.Context, id peer.ID) (peer.AddrInfo, error) {
	return b.getDHT().FindPeer(ctx, id)
}

func (b *bundledDHT) PutValue(ctx context.Context, k string, v []byte, option ...routing.Option) error {
	return b.getDHT().PutValue(ctx, k, v, option...)
}

func (b *bundledDHT) GetValue(ctx context.Context, s string, option ...routing.Option) ([]byte, error) {
	return b.getDHT().GetValue(ctx, s, option...)
}

func (b *bundledDHT) SearchValue(ctx context.Context, s string, option ...routing.Option) (<-chan []byte, error) {
	return b.getDHT().SearchValue(ctx, s, option...)
}

func (b *bundledDHT) Bootstrap(ctx context.Context) error {
	return b.standard.Bootstrap(ctx)
}

var _ routing.Routing = (*bundledDHT)(nil)

func delegatedHTTPContentRouter(endpoint string, rv1Opts ...routingv1client.Option) (routing.Routing, error) {
	cli, err := routingv1client.New(
		endpoint,
		append([]routingv1client.Option{
			routingv1client.WithUserAgent(buildVersion()),
		}, rv1Opts...)...,
	)
	if err != nil {
		return nil, err
	}

	cr := httpcontentrouter.NewContentRoutingClient(
		cli,
	)

	err = view.Register(routingv1client.OpenCensusViews...)
	if err != nil {
		return nil, fmt.Errorf("registering HTTP delegated routing views: %w", err)
	}

	return &routinghelpers.Compose{
		ValueStore:     cr,
		PeerRouting:    cr,
		ContentRouting: cr,
	}, nil
}

func loadOrInitPeerKey(kf string) (crypto.PrivKey, error) {
	data, err := os.ReadFile(kf)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}

		k, _, err := crypto.GenerateEd25519Key(crand.Reader)
		if err != nil {
			return nil, err
		}

		data, err := crypto.MarshalPrivateKey(k)
		if err != nil {
			return nil, err
		}

		if err := os.WriteFile(kf, data, 0600); err != nil {
			return nil, err
		}

		return k, nil
	}
	return crypto.UnmarshalPrivateKey(data)
}
