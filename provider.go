package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	goruntime "runtime"

	voucher "github.com/filecoin-project/data-transfer-benchmark/voucher"
	datatransfer "github.com/filecoin-project/go-data-transfer"
	"github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-graphsync/storeutil"
	chunk "github.com/ipfs/go-ipfs-chunker"
	offline "github.com/ipfs/go-ipfs-exchange-offline"
	files "github.com/ipfs/go-ipfs-files"
	format "github.com/ipfs/go-ipld-format"
	"github.com/ipfs/go-merkledag"
	"github.com/ipfs/go-unixfs/importer/balanced"
	ihelper "github.com/ipfs/go-unixfs/importer/helpers"
	carv2 "github.com/ipld/go-car/v2"
	"github.com/ipld/go-car/v2/blockstore"
	"github.com/ipld/go-ipld-prime"
	"github.com/libp2p/go-libp2p-core/host"
	gostream "github.com/libp2p/go-libp2p-gostream"
	p2phttp "github.com/libp2p/go-libp2p-http"
	"github.com/testground/sdk-go/network"
	"github.com/testground/sdk-go/run"
	"github.com/testground/sdk-go/runtime"
	"github.com/testground/sdk-go/sync"
)

// runProvider is the test case for the provider
func runProvider(ctx context.Context, runenv *runtime.RunEnv, initCtx *run.InitContext, dt datatransfer.Manager, size uint64, h host.Host, ip net.IP, networkParams []networkParams, concurrency int, memorySnapshots snapshotMode, recorder *runRecorder) error {

	runHTTPTest := runenv.BooleanParam("compare_http")
	runLibp2pHTTPTest := runenv.BooleanParam("compare_libp2p_http")
	var httpSvr *http.Server
	var libp2pHTTPSvr *http.Server
	if runHTTPTest {
		runenv.RecordMessage("creating http server at http://%s:8080", ip.String())
		httpSvr = &http.Server{Addr: ":8080"}

		go func() {
			if err := httpSvr.ListenAndServe(); err != nil {
				runenv.RecordMessage("shutdown http server at http://%s:8080", ip.String())
			}
		}()
	}
	if runLibp2pHTTPTest {
		listener, _ := gostream.Listen(h, p2phttp.DefaultP2PProtocol)
		defer listener.Close()
		// start an http server on port 8080
		runenv.RecordMessage("creating libp2p http server at libp2p://%s", h.ID().String())
		libp2pHTTPSvr = &http.Server{}
		go func() {
			if err := libp2pHTTPSvr.Serve(listener); err != nil {
				runenv.RecordMessage("shutdown libp2p http server at libp2p://%s", h.ID().String())
			}
		}()
	}
	stores := make(map[cid.Cid]string)
	for round, np := range networkParams {
		var (
			topicCid    = sync.NewTopic(fmt.Sprintf("cid-%d", round), []cid.Cid{})
			stateNext   = sync.State(fmt.Sprintf("next-%d", round))
			stateFinish = sync.State(fmt.Sprintf("finish-%d", round))
			stateNet    = sync.State(fmt.Sprintf("network-configured-%d", round))
		)

		// wait for all instances to be ready for the next state.
		initCtx.SyncClient.MustSignalAndWait(ctx, stateNext, runenv.TestInstanceCount)
		recorder.beginRun(np, size, concurrency, round)
		cids := make([]cid.Cid, 0, concurrency)
		var bstores []ClosableBlockstore
		getStore := func(c cid.Cid) (ipld.LinkSystem, error) {
			store, ok := stores[c]
			if !ok {
				return ipld.LinkSystem{}, fmt.Errorf("no store for cid: %w", c)
			}
			bs, err := blockstore.OpenReadOnly(store, carv2.ZeroLengthSectionAsEOF(true),
				blockstore.UseWholeCIDs(true))
			if err != nil {
				return ipld.LinkSystem{}, fmt.Errorf("attempting to setup blockstore: %w", err)
			}
			bstores = append(bstores, bs)
			return storeutil.LinkSystemForBlockstore(bs), nil
		}
		dt.RegisterTransportConfigurer(&voucher.Voucher{}, TransportConfigurer(runenv, getStore))

		// generate as many random files as the concurrency level.
		for i := 0; i < concurrency; i++ {
			// file with random data
			data := io.LimitReader(rand.Reader, int64(size))
			file := files.NewReaderFile(data)
			carFile, err := ioutil.TempFile(os.TempDir(), "fsindex-")
			if err != nil {
				panic(err)
			}
			n := carFile.Name()
			err = carFile.Close()
			if err != nil {
				panic(err)
			}
			bs, err := blockstore.OpenReadWrite(n, nil,
				carv2.ZeroLengthSectionAsEOF(true),
				blockstore.UseWholeCIDs(true))
			defer func() {
				os.Remove(n)
			}()
			if err != nil {
				panic(err)
			}
			bsvc := blockservice.New(bs, offline.Exchange(bs))
			dags := merkledag.NewDAGService(bsvc)
			fileDS := format.NewBufferedDAG(ctx, dags)
			unixfsChunkSize := uint64(1) << runenv.IntParam("chunk_size")
			unixfsLinksPerLevel := runenv.IntParam("links_per_level")

			params := ihelper.DagBuilderParams{
				Maxlinks:   unixfsLinksPerLevel,
				RawLeaves:  runenv.BooleanParam("raw_leaves"),
				CidBuilder: nil,
				Dagserv:    fileDS,
			}

			db, err := params.New(chunk.NewSizeSplitter(file, int64(unixfsChunkSize)))
			if err != nil {
				return fmt.Errorf("unable to setup dag builder: %w", err)
			}

			node, err := balanced.Layout(db)
			if err != nil {
				return fmt.Errorf("unable to create unix fs node: %w", err)
			}

			if err := fileDS.Commit(); err != nil {
				return fmt.Errorf("unable to commit unix fs node: %w", err)
			}

			err = bs.Finalize()
			if err != nil {
				panic(err)
			}
			stores[node.Cid()] = n

			cids = append(cids, node.Cid())
		}

		// run GC to get accurate-ish stats.
		if memorySnapshots == snapshotSimple || memorySnapshots == snapshotDetailed {
			recordSnapshots(runenv, size, np, concurrency, "pre")
		}
		goruntime.GC()

		// setup http to serve
		if runHTTPTest {
			for c, n := range stores {
				// set up http server to send file
				http.HandleFunc(fmt.Sprintf("/%s", c), func(w http.ResponseWriter, r *http.Request) {
					fileReader, err := os.Open(n)
					if err != nil {
						panic(err)
					}
					defer fileReader.Close()
					_, err = io.Copy(w, fileReader)
					if err != nil {
						panic(err)
					}
				})
			}
		}
		runenv.RecordMessage("\tCIDs are: %v", cids)
		initCtx.SyncClient.MustPublish(ctx, topicCid, cids)

		if runenv.TestSidecar {
			runenv.RecordMessage("\tconfiguring network for round %d", round)
			initCtx.NetClient.MustConfigureNetwork(ctx, &network.Config{
				Network: "default",
				Enable:  true,
				Default: network.LinkShape{
					Latency:   np.latency,
					Bandwidth: np.bandwidth * 8, // bps
				},
				CallbackState:  stateNet,
				CallbackTarget: 1,
			})
			runenv.RecordMessage("\tnetwork configured for round %d", round)
		}
		// wait for all instances to finish running
		initCtx.SyncClient.MustSignalAndWait(ctx, stateFinish, runenv.TestInstanceCount)

		if memorySnapshots == snapshotSimple || memorySnapshots == snapshotDetailed {
			recordSnapshots(runenv, size, np, concurrency, "total")
		}

		for _, bstore := range bstores {
			bstore.Close()
		}
		bstores = nil
		for c := range stores {
			delete(stores, c)
		}
	}

	if runHTTPTest {
		if err := httpSvr.Shutdown(ctx); err != nil {
			panic(err)
		}
	}
	if runLibp2pHTTPTest {
		if err := libp2pHTTPSvr.Shutdown(ctx); err != nil {
			panic(err)
		}
	}
	return nil
}
