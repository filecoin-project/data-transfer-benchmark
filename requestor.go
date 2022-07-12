package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	gosync "sync"
	"time"

	voucher "github.com/filecoin-project/data-transfer-benchmark/voucher"
	datatransfer "github.com/filecoin-project/go-data-transfer"
	"github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-graphsync/storeutil"
	offline "github.com/ipfs/go-ipfs-exchange-offline"
	format "github.com/ipfs/go-ipld-format"
	"github.com/ipfs/go-merkledag"
	"github.com/ipld/go-ipld-prime"
	selectorparse "github.com/ipld/go-ipld-prime/traversal/selector/parse"
	"github.com/libp2p/go-libp2p-core/host"
	p2phttp "github.com/libp2p/go-libp2p-http"
	"github.com/testground/sdk-go/run"
	"github.com/testground/sdk-go/runtime"
	"github.com/testground/sdk-go/sync"
	"golang.org/x/sync/errgroup"
)

// runProvider is the test case for the requestor
func runRequestor(ctx context.Context, runenv *runtime.RunEnv, initCtx *run.InitContext, dt datatransfer.Manager, h host.Host, p *AddrInfo, networkParams []networkParams, concurrency int, size uint64, memorySnapshots snapshotMode, recorder *runRecorder) error {
	var (
		// create a selector for the whole UnixFS dag
		sel = selectorparse.CommonSelector_ExploreAllRecursively
	)

	runHTTPTest := runenv.BooleanParam("compare_http")
	runLibp2pHTTPTest := runenv.BooleanParam("compare_libp2p_http")

	var libp2pHTTPClient *http.Client
	if runLibp2pHTTPTest {
		tr := &http.Transport{}
		tr.RegisterProtocol("libp2p", p2phttp.NewTransport(h))
		libp2pHTTPClient = &http.Client{Transport: tr}
	}

	rwBlockstores := NewReadWriteBlockstores()
	stores := make(map[cid.Cid]ipld.LinkSystem)
	dagSrvs := make(map[cid.Cid]format.DAGService)

	dt.RegisterTransportConfigurer(&voucher.Voucher{}, TransportConfigurer(runenv, func(c cid.Cid) (ipld.LinkSystem, error) {
		lsys, ok := stores[c]
		if !ok {
			return ipld.LinkSystem{}, fmt.Errorf("no blockstore for cid: %s", c.String())
		}
		return lsys, nil
	}))

	for round, np := range networkParams {
		var (
			topicCid    = sync.NewTopic(fmt.Sprintf("cid-%d", round), []cid.Cid{})
			stateNext   = sync.State(fmt.Sprintf("next-%d", round))
			stateNet    = sync.State(fmt.Sprintf("network-configured-%d", round))
			stateFinish = sync.State(fmt.Sprintf("finish-%d", round))
		)

		// wait for all instances to be ready for the next state.
		initCtx.SyncClient.MustSignalAndWait(ctx, stateNext, runenv.TestInstanceCount)
		recorder.beginRun(np, size, concurrency, round)

		// clean up previous CIDs to attempt to free memory
		// TODO does this work?

		sctx, scancel := context.WithCancel(ctx)
		cidCh := make(chan []cid.Cid, 1)
		initCtx.SyncClient.MustSubscribe(sctx, topicCid, cidCh)
		cids := <-cidCh
		scancel()

		for _, c := range cids {
			bs, err := rwBlockstores.GetOrOpen(c.String(), filepath.Join(os.TempDir(), c.String()), c)
			if err != nil {
				panic(err)
			}
			lsys := storeutil.LinkSystemForBlockstore(bs)
			stores[c] = lsys
			bsvc := blockservice.New(bs, offline.Exchange(bs))
			dagSrvs[c] = merkledag.NewDAGService(bsvc)
			defer func(c cid.Cid) {
				bs.Finalize()
				os.Remove(filepath.Join(os.TempDir(), c.String()))
			}(c)
		}

		// run GC to get accurate-ish stats.
		goruntime.GC()
		goruntime.GC()

		if runenv.TestSidecar {
			<-initCtx.SyncClient.MustBarrier(ctx, stateNet, 1).C
		}

		errgrp, grpctx := errgroup.WithContext(ctx)

		for _, c := range cids {
			c := c // capture

			errgrp.Go(func() error {

				// execute the traversal.
				runenv.RecordMessage("\t>>> requesting CID %s", c)

				var chanid datatransfer.ChannelID
				var chanidLk gosync.Mutex

				// dtRes receives either an error (failure) or nil (success) which is waited
				// on and handled below before exiting the function
				dtRes := make(chan error, 1)

				finish := func(err error) {
					select {
					case dtRes <- err:
					default:
					}
				}

				unsubscribe := dt.SubscribeToEvents(func(event datatransfer.Event, state datatransfer.ChannelState) {
					// Copy chanid so it can be used later in the callback
					chanidLk.Lock()
					chanidCopy := chanid
					chanidLk.Unlock()

					// Skip all events that aren't related to this channel
					if state.ChannelID() != chanidCopy {
						return
					}
					if state.Status() == datatransfer.Failed || state.Status() == datatransfer.Cancelled {
						finish(errors.New(state.Message()))
					}
					if state.Status() == datatransfer.Completed {
						finish(nil)
					}
				})
				defer unsubscribe()
				start := time.Now()

				newchid, err := dt.OpenPullDataChannel(ctx, p.peerAddr.ID, &voucher.Voucher{C: &c}, c, sel)

				if err != nil {
					return err
				}

				chanidLk.Lock()
				chanid = newchid
				chanidLk.Unlock()
				select {
				case err := <-dtRes:
					if err != nil {
						return fmt.Errorf("data transfer failed: %w", err)
					}
				case <-ctx.Done():
					return ctx.Err()
				}

				dur := time.Since(start)

				recorder.recordRun(dur)
				// verify that we have the CID now.

				if node, err := dagSrvs[c].Get(grpctx, c); err != nil {
					return err
				} else if node == nil {
					return fmt.Errorf("finished graphsync request, but CID not in store")
				}
				return nil
			})
		}

		if err := errgrp.Wait(); err != nil {
			return err
		}

		if runHTTPTest {

			for _, c := range cids {
				c := c // capture
				errgrp.Go(func() error {
					// request file directly over http
					start := time.Now()
					resp, err := http.DefaultClient.Get(fmt.Sprintf("http://%s:8080/%s", p.ip.String(), c.String()))
					if err != nil {
						panic(err)
					}
					bytesRead, err := io.Copy(ioutil.Discard, resp.Body)
					if err != nil {
						panic(err)
					}
					dur := time.Since(start)
					recorder.recordHTTPRun(dur, bytesRead)
					return nil
				})
			}

			if err := errgrp.Wait(); err != nil {
				return err
			}
		}
		if runLibp2pHTTPTest {

			for _, c := range cids {
				c := c // capture
				errgrp.Go(func() error {
					// request file directly over http over libp2p
					start := time.Now()
					resp, err := libp2pHTTPClient.Get(fmt.Sprintf("libp2p://%s/%s", p.peerAddr.ID.String(), c.String()))
					if err != nil {
						panic(err)
					}
					bytesRead, err := io.Copy(ioutil.Discard, resp.Body)
					if err != nil {
						panic(err)
					}
					dur := time.Since(start)
					recorder.recordLibp2pHTTPRun(dur, bytesRead)

					return nil
				})
			}

			if err := errgrp.Wait(); err != nil {
				return err
			}
		}
		// wait for all instances to finish running
		initCtx.SyncClient.MustSignalAndWait(ctx, stateFinish, runenv.TestInstanceCount)

		if memorySnapshots == snapshotSimple || memorySnapshots == snapshotDetailed {
			recordSnapshots(runenv, size, np, concurrency, "total")
		}
	}

	return nil
}
