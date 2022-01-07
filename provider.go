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
func runProvider(ctx context.Context, runenv *runtime.RunEnv, initCtx *run.InitContext, dt datatransfer.Manager, dagsrv format.DAGService, size uint64, h host.Host, ip net.IP, networkParams []networkParams, concurrency int, memorySnapshots snapshotMode, useCarStores bool, recorder *runRecorder) error {
	var (
		cids       []cid.Cid
		bufferedDS = format.NewBufferedDAG(ctx, dagsrv)
	)

	runHTTPTest := runenv.BooleanParam("compare_http")
	useLibP2p := runenv.BooleanParam("use_libp2p_http")
	var svr *http.Server
	if runHTTPTest {
		if useLibP2p {
			listener, _ := gostream.Listen(h, p2phttp.DefaultP2PProtocol)
			defer listener.Close()
			// start an http server on port 8080
			runenv.RecordMessage("creating http server at libp2p://%s", h.ID().String())
			svr = &http.Server{}
			go func() {
				if err := svr.Serve(listener); err != nil {
					runenv.RecordMessage("shutdown http server at libp2p://%s", h.ID().String())
				}
			}()
		} else {
			runenv.RecordMessage("creating http server at http://%s:8080", ip.String())
			svr = &http.Server{Addr: ":8080"}

			go func() {
				if err := svr.ListenAndServe(); err != nil {
					runenv.RecordMessage("shutdown http server at http://%s:8080", ip.String())
				}
			}()
		}
	}

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

		// remove the previous CIDs from the dag service; hopefully this
		// will delete them from the store and free up memory.
		for _, c := range cids {
			_ = dagsrv.Remove(ctx, c)
		}
		cids = cids[:0]
		stores := make(map[cid.Cid]ipld.LinkSystem)

		// generate as many random files as the concurrency level.
		for i := 0; i < concurrency; i++ {
			// file with random data
			data := io.LimitReader(rand.Reader, int64(size))
			f, err := ioutil.TempFile(os.TempDir(), "unixfs-")
			defer func() {
				name := f.Name()
				f.Close()
				os.Remove(name)
			}()
			if err != nil {
				panic(err)
			}
			if _, err := io.Copy(f, data); err != nil {
				panic(err)
			}
			_, err = f.Seek(0, 0)
			if err != nil {
				panic(err)
			}
			stat, err := f.Stat()
			if err != nil {
				panic(err)
			}
			file, err := files.NewReaderPathFile(f.Name(), f, stat)
			if err != nil {
				panic(err)
			}
			var bs ClosableBlockstore
			var fileDS = bufferedDS
			if useCarStores {
				filestore, err := ioutil.TempFile(os.TempDir(), "fsindex-")
				if err != nil {
					panic(err)
				}
				n := filestore.Name()
				err = filestore.Close()
				if err != nil {
					panic(err)
				}
				bs, err = ReadWriteFilestore(n)
				defer func() {
					bs.Close()
					os.Remove(n)
				}()
				if err != nil {
					panic(err)
				}
				bsvc := blockservice.New(bs, offline.Exchange(bs))
				dags := merkledag.NewDAGService(bsvc)
				fileDS = format.NewBufferedDAG(ctx, dags)
			}
			unixfsChunkSize := uint64(1) << runenv.IntParam("chunk_size")
			unixfsLinksPerLevel := runenv.IntParam("links_per_level")

			params := ihelper.DagBuilderParams{
				Maxlinks:   unixfsLinksPerLevel,
				RawLeaves:  runenv.BooleanParam("raw_leaves"),
				CidBuilder: nil,
				Dagserv:    fileDS,
			}

			if _, err := file.Seek(0, 0); err != nil {
				panic(err)
			}
			db, err := params.New(chunk.NewSizeSplitter(file, int64(unixfsChunkSize)))
			if err != nil {
				return fmt.Errorf("unable to setup dag builder: %w", err)
			}

			node, err := balanced.Layout(db)
			if err != nil {
				return fmt.Errorf("unable to create unix fs node: %w", err)
			}

			if runHTTPTest {
				// set up http server to send file
				http.HandleFunc(fmt.Sprintf("/%s", node.Cid()), func(w http.ResponseWriter, r *http.Request) {
					fileReader, err := os.Open(f.Name())
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
			if useCarStores {
				lsys := storeutil.LinkSystemForBlockstore(bs)
				stores[node.Cid()] = lsys
				if err := fileDS.Commit(); err != nil {
					return fmt.Errorf("unable to commit unix fs node: %w", err)
				}
			}
			cids = append(cids, node.Cid())
		}

		if err := bufferedDS.Commit(); err != nil {
			return fmt.Errorf("unable to commit unix fs node: %w", err)
		}

		// run GC to get accurate-ish stats.
		if memorySnapshots == snapshotSimple || memorySnapshots == snapshotDetailed {
			recordSnapshots(runenv, size, np, concurrency, "pre")
		}
		goruntime.GC()

		if useCarStores {
			dt.RegisterTransportConfigurer(&voucher.Voucher{}, TransportConfigurer(runenv, stores))
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

	}

	if runHTTPTest {
		if err := svr.Shutdown(ctx); err != nil {
			panic(err)
		}
	}
	return nil
}
