package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	goruntime "runtime"
	"runtime/pprof"
	"strings"
	"time"

	dgbadger "github.com/dgraph-io/badger/v2"
	"github.com/dustin/go-humanize"
	voucher "github.com/filecoin-project/data-transfer-benchmark/voucher"
	datatransfer "github.com/filecoin-project/go-data-transfer"
	"github.com/ipfs/go-cid"
	ds "github.com/ipfs/go-datastore"
	badgerds "github.com/ipfs/go-ds-badger2"
	"github.com/ipld/go-ipld-prime"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/metrics"
	"github.com/libp2p/go-libp2p-core/peer"
	noise "github.com/libp2p/go-libp2p-noise"
	tls "github.com/libp2p/go-libp2p-tls"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/testground/sdk-go/run"
	"github.com/testground/sdk-go/runtime"
	"github.com/testground/sdk-go/sync"
)

type networkParams struct {
	latency   time.Duration
	bandwidth uint64
}

func (p networkParams) String() string {
	return fmt.Sprintf("<lat: %s, bandwidth: %d>", p.latency, p.bandwidth)
}

func parseNetworkConfig(runenv *runtime.RunEnv) []networkParams {
	var (
		bandwidths = runenv.SizeArrayParam("bandwidths")
		latencies  []time.Duration
	)

	lats := runenv.StringArrayParam("latencies")
	for _, l := range lats {
		d, err := time.ParseDuration(l)
		if err != nil {
			panic(err)
		}
		latencies = append(latencies, d)
	}

	// prepend bandwidth=0 and latency=0 zero values; the first iteration will
	// be a control iteration. The sidecar interprets zero values as no
	// limitation on that attribute.
	if runenv.BooleanParam("unlimited_bandwidth_case") {
		bandwidths = append([]uint64{0}, bandwidths...)
	}
	if runenv.BooleanParam("no_latency_case") {
		latencies = append([]time.Duration{0}, latencies...)
	}

	var ret []networkParams
	for _, bandwidth := range bandwidths {
		for _, latency := range latencies {
			ret = append(ret, networkParams{
				latency:   latency,
				bandwidth: bandwidth,
			})
		}
	}
	return ret
}

func parseMemorySnapshotsParam(runenv *runtime.RunEnv) snapshotMode {
	memorySnapshotsString := runenv.StringParam("memory_snapshots")
	switch memorySnapshotsString {
	case "none":
		return snapshotNone
	case "simple":
		return snapshotSimple
	case "detailed":
		return snapshotDetailed
	default:
		panic("invalid memory_snapshot parameter")
	}
}

func makeHost(ctx context.Context, runenv *runtime.RunEnv, initCtx *run.InitContext) (host.Host, net.IP, []*AddrInfo, *metrics.BandwidthCounter) {
	secureChannel := runenv.StringParam("secure_channel")

	var security libp2p.Option
	switch secureChannel {
	case "noise":
		security = libp2p.Security(noise.ID, noise.New)
	case "tls":
		security = libp2p.Security(tls.ID, tls.New)
	}

	// ☎️  Let's construct the libp2p node.
	ip := initCtx.NetClient.MustGetDataNetworkIP()
	listenAddr := fmt.Sprintf("/ip4/%s/tcp/0", ip)
	bwcounter := metrics.NewBandwidthCounter()
	host, err := libp2p.New(
		security,
		libp2p.ListenAddrStrings(listenAddr),
		libp2p.BandwidthReporter(bwcounter),
	)
	if err != nil {
		panic(fmt.Sprintf("failed to instantiate libp2p instance: %s", err))
	}

	// Record our listen addrs.
	runenv.RecordMessage("my listen addrs: %v", host.Addrs())

	// Obtain our own address info, and use the sync service to publish it to a
	// 'peersTopic' topic, where others will read from.
	var (
		id = host.ID()
		ai = &peer.AddrInfo{ID: id, Addrs: host.Addrs()}

		// the peers topic where all instances will advertise their AddrInfo.
		peersTopic = sync.NewTopic("peers", new(AddrInfo))

		// initialize a slice to store the AddrInfos of all other peers in the run.
		peers = make([]*AddrInfo, 0, runenv.TestInstanceCount-1)
	)

	// Publish our own.
	initCtx.SyncClient.MustPublish(ctx, peersTopic, &AddrInfo{
		peerAddr: ai,
		ip:       ip,
	})

	// Now subscribe to the peers topic and consume all addresses, storing them
	// in the peers slice.
	peersCh := make(chan *AddrInfo)
	sctx, scancel := context.WithCancel(ctx)
	defer scancel()

	sub := initCtx.SyncClient.MustSubscribe(sctx, peersTopic, peersCh)

	// Receive the expected number of AddrInfos.
	var foundSelf bool
	for len(peers) < cap(peers) || !foundSelf {
		select {
		case ai := <-peersCh:
			if ai.peerAddr.ID == id {
				foundSelf = true
				continue // skip over ourselves.
			}
			peers = append(peers, ai)
		case err := <-sub.Done():
			panic(err)
		}
	}

	return host, ip, peers, bwcounter
}

func createDatastore(diskStore bool) (ds.Datastore, string, error) {
	if !diskStore {
		return ds.NewMapDatastore(), "", nil
	}

	path, err := os.MkdirTemp("", "datastore")
	if err != nil {
		return nil, "", err
	}

	// create disk based badger datastore
	defopts := badgerds.DefaultOptions

	defopts.Options = dgbadger.DefaultOptions("").WithTruncate(true).
		WithValueThreshold(1 << 10)
	datastore, err := badgerds.NewDatastore(path, &defopts)
	if err != nil {
		os.RemoveAll(path)
		return nil, "", err
	}

	return datastore, path, nil
}

func recordSnapshots(runenv *runtime.RunEnv, size uint64, np networkParams, concurrency int, postfix string) error {
	runenv.RecordMessage("Recording heap profile...")
	err := writeHeap(runenv, size, np, concurrency, fmt.Sprintf("%s-pre-gc", postfix))
	if err != nil {
		return err
	}
	goruntime.GC()
	goruntime.GC()
	err = writeHeap(runenv, size, np, concurrency, fmt.Sprintf("%s-post-gc", postfix))
	if err != nil {
		return err
	}
	return nil
}

func writeHeap(runenv *runtime.RunEnv, size uint64, np networkParams, concurrency int, postfix string) error {
	snapshotName := fmt.Sprintf("heap_lat-%s_bw-%s_concurrency-%d_size-%s_%s", np.latency, humanize.IBytes(np.bandwidth), concurrency, humanize.Bytes(size), postfix)
	snapshotName = strings.ReplaceAll(snapshotName, " ", "")
	snapshotFile, err := runenv.CreateRawAsset(snapshotName)
	if err != nil {
		return err
	}
	err = pprof.WriteHeapProfile(snapshotFile)
	if err != nil {
		return err
	}
	err = snapshotFile.Close()
	if err != nil {
		return err
	}
	return nil
}

func startAndWaitForReady(ctx context.Context, manager datatransfer.Manager) error {
	ready := make(chan error, 1)
	manager.OnReady(func(err error) {
		ready <- err
	})
	err := manager.Start(ctx)
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return errors.New("did not finish starting up module")
	case err := <-ready:
		return err
	}
}

// StoreConfigurableTransport defines the methods needed to
// configure a data transfer transport use a unique store for a given request
type StoreConfigurableTransport interface {
	UseStore(datatransfer.ChannelID, ipld.LinkSystem) error
}

// TransportConfigurer configurers the graphsync transport to use a custom blockstore per deal
func TransportConfigurer(runenv *runtime.RunEnv, stores map[cid.Cid]ipld.LinkSystem) datatransfer.TransportConfigurer {

	return func(channelID datatransfer.ChannelID, v datatransfer.Voucher, transport datatransfer.Transport) {
		myVouch, ok := v.(*voucher.Voucher)
		if !ok {
			return
		}
		gsTransport, ok := transport.(StoreConfigurableTransport)
		if !ok {
			return
		}
		store, ok := stores[*myVouch.C]
		if !ok {
			runenv.RecordMessage("no store for cid: %s", *myVouch.C)
			return
		}
		err := gsTransport.UseStore(channelID, store)
		if err != nil {
			runenv.RecordMessage("attempting to configure data store: %s", err)
		}
	}
}

type AddrInfo struct {
	peerAddr *peer.AddrInfo
	ip       net.IP
}

func (pi AddrInfo) MarshalJSON() ([]byte, error) {
	out := make(map[string]interface{})
	peerJSON, err := pi.peerAddr.MarshalJSON()
	if err != nil {
		panic(fmt.Sprintf("error marshaling: %v", err))
	}
	out["PEER"] = string(peerJSON)

	ip, err := pi.ip.MarshalText()
	if err != nil {
		panic(fmt.Sprintf("error marshaling: %v", err))
	}
	out["IP"] = string(ip)
	return json.Marshal(out)
}

func (pi *AddrInfo) UnmarshalJSON(b []byte) error {
	var data map[string]interface{}
	err := json.Unmarshal(b, &data)
	if err != nil {
		panic(fmt.Sprintf("error unmarshaling: %v", err))
	}

	var pa peer.AddrInfo
	pi.peerAddr = &pa
	peerAddrData := data["PEER"].(string)
	var peerData map[string]interface{}
	err = json.Unmarshal([]byte(peerAddrData), &peerData)
	if err != nil {
		panic(err)
	}
	pid, err := peer.Decode(peerData["ID"].(string))
	if err != nil {
		panic(err)
	}
	pi.peerAddr.ID = pid
	addrs, ok := peerData["Addrs"].([]interface{})
	if ok {
		for _, a := range addrs {
			pi.peerAddr.Addrs = append(pi.peerAddr.Addrs, ma.StringCast(a.(string)))
		}
	}

	if err := pi.ip.UnmarshalText([]byte(data["IP"].(string))); err != nil {
		panic(fmt.Sprintf("error unmarshaling: %v", err))
	}
	return nil
}
