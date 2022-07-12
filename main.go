package main

import (
	"context"
	"fmt"
	"os"
	gosync "sync"
	"time"

	ds "github.com/ipfs/go-datastore"
	"github.com/ipfs/go-datastore/namespace"
	dss "github.com/ipfs/go-datastore/sync"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/testground/sdk-go/run"
	"github.com/testground/sdk-go/runtime"

	voucher "github.com/filecoin-project/data-transfer-benchmark/voucher"
	dtimpl "github.com/filecoin-project/go-data-transfer/impl"
	dtnetwork "github.com/filecoin-project/go-data-transfer/network"
	gst "github.com/filecoin-project/go-data-transfer/transport/graphsync"
	gs "github.com/ipfs/go-graphsync"
	gsi "github.com/ipfs/go-graphsync/impl"
	gsnet "github.com/ipfs/go-graphsync/network"
	"github.com/ipfs/go-graphsync/storeutil"
)

var testcases = map[string]interface{}{
	"stress": run.InitializedTestCaseFn(runStress),
}

func main() {
	run.InvokeMap(testcases)
}

func runStress(runenv *runtime.RunEnv, initCtx *run.InitContext) error {
	var (
		size             = runenv.SizeParam("size")
		concurrency      = runenv.IntParam("concurrency")
		blockDiagnostics = runenv.BooleanParam("block_diagnostics")
		networkParams    = parseNetworkConfig(runenv)
		memorySnapshots  = parseMemorySnapshotsParam(runenv)
	)
	runenv.RecordMessage("started test instance")
	runenv.RecordMessage("network params: %v", networkParams)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	initCtx.MustWaitAllInstancesInitialized(ctx)

	jaeger, err := setupJaegerTracing(runenv, fmt.Sprintf("data-transfer-benchmark-%s", runenv.TestGroupID))
	if err != nil {
		return err
	}
	defer func() {
		if jaeger != nil {
			_ = jaeger.ForceFlush(context.Background())
		}
	}()

	host, ip, peers, _ := makeHost(ctx, runenv, initCtx)
	defer host.Close()

	datastore, dsPath, err := createDatastore(runenv.BooleanParam("disk_store"))
	if err != nil {
		runenv.RecordMessage("datastore error: %s", err.Error())
		return err
	}
	defer func() {
		datastore.Close()
		if dsPath != "" {
			os.RemoveAll(dsPath)
		}
	}()
	maxMemoryPerPeer := runenv.SizeParam("max_memory_per_peer")
	maxMemoryTotal := runenv.SizeParam("max_memory_total")
	maxInProgressRequests := runenv.IntParam("max_in_progress_requests")
	var (
		// make datastore, blockstore, dag service, graphsync
		bs    = blockstore.NewBlockstore(dss.MutexWrap(datastore))
		gsync = gsi.New(ctx,
			gsnet.NewFromLibp2pHost(host),
			storeutil.LinkSystemForBlockstore(bs),
			gsi.MaxMemoryPerPeerResponder(maxMemoryPerPeer),
			gsi.MaxMemoryResponder(maxMemoryTotal),
			gsi.MaxInProgressIncomingRequests(uint64(maxInProgressRequests)),
			gsi.MaxInProgressOutgoingRequests(uint64(maxInProgressRequests)),
		)
		recorder  = &runRecorder{memorySnapshots: memorySnapshots, blockDiagnostics: blockDiagnostics, runenv: runenv}
		dtNet     = dtnetwork.NewFromLibp2pHost(host)
		transport = gst.NewTransport(host.ID(), gsync)
	)
	dt, err := dtimpl.NewDataTransfer(namespace.Wrap(dss.MutexWrap(datastore), ds.NewKey("/datatransfer/client/transfers")), dtNet, transport)
	if err != nil {
		return err
	}
	err = startAndWaitForReady(ctx, dt)
	if err != nil {
		return err
	}
	dt.RegisterVoucherType(&voucher.Voucher{}, &voucher.Validator{})
	dt.RegisterVoucherResultType(&voucher.VoucherResult{})
	startTimes := make(map[struct {
		peer.ID
		gs.RequestID
	}]time.Time)
	var startTimesLk gosync.RWMutex
	gsync.RegisterIncomingRequestHook(func(p peer.ID, request gs.RequestData, hookActions gs.IncomingRequestHookActions) {
		startTimesLk.Lock()
		startTimes[struct {
			peer.ID
			gs.RequestID
		}{p, request.ID()}] = time.Now()
		startTimesLk.Unlock()
	})
	gsync.RegisterOutgoingRequestHook(func(p peer.ID, request gs.RequestData, hookActions gs.OutgoingRequestHookActions) {
		startTimesLk.Lock()
		startTimes[struct {
			peer.ID
			gs.RequestID
		}{p, request.ID()}] = time.Now()
		startTimesLk.Unlock()
	})
	gsync.RegisterCompletedResponseListener(func(p peer.ID, request gs.RequestData, status gs.ResponseStatusCode) {
		startTimesLk.RLock()
		startTime, ok := startTimes[struct {
			peer.ID
			gs.RequestID
		}{p, request.ID()}]
		startTimesLk.RUnlock()
		if ok && status == gs.RequestCompletedFull {
			duration := time.Since(startTime)
			recorder.recordRun(duration)
		}
	})
	gsync.RegisterOutgoingBlockHook(func(p peer.ID, request gs.RequestData, block gs.BlockData, ha gs.OutgoingBlockHookActions) {
		startTimesLk.RLock()
		startTime := startTimes[struct {
			peer.ID
			gs.RequestID
		}{p, request.ID()}]
		startTimesLk.RUnlock()
		recorder.recordBlockQueued(fmt.Sprintf("for request %d at %s", request.ID(), time.Since(startTime)))
	})
	gsync.RegisterBlockSentListener(func(p peer.ID, request gs.RequestData, block gs.BlockData) {
		startTimesLk.RLock()
		startTime := startTimes[struct {
			peer.ID
			gs.RequestID
		}{p, request.ID()}]
		startTimesLk.RUnlock()
		recorder.recordBlock(fmt.Sprintf("sent for request %d at %s", request.ID(), time.Since(startTime)))
	})
	gsync.RegisterIncomingResponseHook(func(p peer.ID, response gs.ResponseData, actions gs.IncomingResponseHookActions) {
		startTimesLk.RLock()
		startTime := startTimes[struct {
			peer.ID
			gs.RequestID
		}{p, response.RequestID()}]
		startTimesLk.RUnlock()
		recorder.recordResponse(fmt.Sprintf("for request %d at %s", response.RequestID(), time.Since(startTime)))
	})
	gsync.RegisterIncomingBlockHook(func(p peer.ID, response gs.ResponseData, block gs.BlockData, ha gs.IncomingBlockHookActions) {
		startTimesLk.RLock()
		startTime := startTimes[struct {
			peer.ID
			gs.RequestID
		}{p, response.RequestID()}]
		startTimesLk.RUnlock()
		recorder.recordBlock(fmt.Sprintf("processed for request %d, cid %s, at %s", response.RequestID(), block.Link().String(), time.Since(startTime)))
	})
	defer initCtx.SyncClient.MustSignalAndWait(ctx, "done", runenv.TestInstanceCount)

	switch runenv.TestGroupID {
	case "providers":
		if runenv.TestGroupInstanceCount > 1 {
			panic("test case only supports one provider")
		}

		runenv.RecordMessage("we are the provider")
		defer runenv.RecordMessage("done provider")
		err := runProvider(ctx, runenv, initCtx, dt, size, host, ip, networkParams, concurrency, memorySnapshots, recorder)
		if err != nil {
			runenv.RecordMessage("Error running provider: %s", err.Error())
		}
		return err
	case "requestors":
		runenv.RecordMessage("we are the requestor")
		defer runenv.RecordMessage("done requestor")

		p := peers[0]
		if err := host.Connect(ctx, *p.peerAddr); err != nil {
			return err
		}
		runenv.RecordMessage("done dialling provider")
		err := runRequestor(ctx, runenv, initCtx, dt, host, p, networkParams, concurrency, size, memorySnapshots, recorder)
		if err != nil {
			runenv.RecordMessage("requestor error: %s", err.Error())
		}
		return err
	default:
		panic(fmt.Sprintf("unsupported group ID: %s\n", runenv.TestGroupID))
	}
}
