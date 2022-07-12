package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/testground/sdk-go/runtime"
)

type snapshotMode uint

const (
	snapshotNone snapshotMode = iota
	snapshotSimple
	snapshotDetailed
)

const (
	detailedSnapshotFrequency = 10
)

// runRecorder fascilates output messages and diagnostics to the console and taking memory snapshots
type runRecorder struct {
	memorySnapshots  snapshotMode
	blockDiagnostics bool
	index            int
	queuedIndex      int
	responseIndex    int
	np               networkParams
	size             uint64
	concurrency      int
	round            int
	runenv           *runtime.RunEnv
	measurement      string
}

func (rr *runRecorder) recordBlock(postfix string) {
	if rr.blockDiagnostics {
		rr.runenv.RecordMessage("block %d %s", rr.index, postfix)
	}
	if rr.memorySnapshots == snapshotDetailed {
		if rr.index%detailedSnapshotFrequency == 0 {
			recordSnapshots(rr.runenv, rr.size, rr.np, rr.concurrency, fmt.Sprintf("incremental-%d", rr.index))
		}
	}
	rr.index++
}

func (rr *runRecorder) recordBlockQueued(postfix string) {
	if rr.blockDiagnostics {
		rr.runenv.RecordMessage("block %d queued %s", rr.queuedIndex, postfix)
	}
	rr.queuedIndex++
}

func (rr *runRecorder) recordResponse(postfix string) {
	if rr.blockDiagnostics {
		rr.runenv.RecordMessage("response %d received %s", rr.responseIndex, postfix)
	}
	rr.responseIndex++
}

func (rr *runRecorder) recordRun(duration time.Duration) {
	rr.runenv.RecordMessage("\t<<< graphsync request complete with no errors")
	rr.runenv.RecordMessage("***** ROUND %d observed duration (lat=%s,bw=%d): %s", rr.round, rr.np.latency, rr.np.bandwidth, duration)
	rr.runenv.R().RecordPoint(rr.measurement+",transport=graphsync", float64(duration)/float64(time.Second))
}

func (rr *runRecorder) recordLibp2pHTTPRun(duration time.Duration, bytesRead int64) {
	rr.runenv.RecordMessage(fmt.Sprintf("\t<<< http over libp2p request complete with no errors, read %d bytes", bytesRead))
	rr.runenv.RecordMessage("***** ROUND %d observed http over libp2p duration (lat=%s,bw=%d): %s", rr.round, rr.np.latency, rr.np.bandwidth, duration)
	rr.runenv.R().RecordPoint(rr.measurement+",transport=http", float64(duration)/float64(time.Second))
}

func (rr *runRecorder) recordHTTPRun(duration time.Duration, bytesRead int64) {
	rr.runenv.RecordMessage(fmt.Sprintf("\t<<< http request complete with no errors, read %d bytes", bytesRead))
	rr.runenv.RecordMessage("***** ROUND %d observed http duration (lat=%s,bw=%d): %s", rr.round, rr.np.latency, rr.np.bandwidth, duration)
	rr.runenv.R().RecordPoint(rr.measurement+",transport=http", float64(duration)/float64(time.Second))
}

func (rr *runRecorder) beginRun(np networkParams, size uint64, concurrency int, round int) {

	rr.concurrency = concurrency
	rr.np = np
	rr.size = size
	rr.index = 0
	rr.round = round
	rr.runenv.RecordMessage("===== ROUND %d: latency=%s, bandwidth=%d =====", rr.round, rr.np.latency, rr.np.bandwidth)
	measurement := fmt.Sprintf("duration.sec,lat=%s,bw=%s,concurrency=%d,size=%s", rr.np.latency, humanize.IBytes(rr.np.bandwidth), rr.concurrency, humanize.Bytes(rr.size))
	measurement = strings.ReplaceAll(measurement, " ", "")
	rr.measurement = measurement
}
