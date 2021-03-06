package consensus

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	cfg "github.com/lianxiangcloud/linkchain/config"
	auto "github.com/lianxiangcloud/linkchain/libs/autofile"
	cmn "github.com/lianxiangcloud/linkchain/libs/common"
	"github.com/lianxiangcloud/linkchain/libs/db"
	"github.com/lianxiangcloud/linkchain/libs/log"
	"github.com/lianxiangcloud/linkchain/types"
	"github.com/pkg/errors"
)

// WALWithNBlocks generates a consensus WAL. It does this by spining up a
// stripped down version of node (event bus, consensus state) with a
// persistent kvstore application and special consensus wal instance
// (byteBufferWAL) and waits until numBlocks are created. Then it returns a WAL
// content.
func WALWithNBlocks(numBlocks uint64) (data []byte, err error) {
	config := getConfig()

	//app := kvstore.NewPersistentKVStoreApplication(filepath.Join(config.DBDir(), "wal_generator"))

	logger := log.Test().With("wal_generator", "wal_generator")
	logger.Info("generating WAL (last height msg excluded)", "numBlocks", numBlocks)

	/////////////////////////////////////////////////////////////////////////////
	// COPY PASTE FROM node.go WITH A FEW MODIFICATIONS
	// NOTE: we can't import node package because of circular dependency
	privValidatorFile := config.PrivValidatorFile()
	privValidator := types.LoadOrGenFilePV(privValidatorFile)
	genDoc, err := types.GenesisDocFromFile(config.GenesisFile())
	if err != nil {
		return nil, errors.Wrap(err, "failed to read genesis file")
	}
	statusDB := db.NewMemDB()
	//blockStoreDB := db.NewMemDB()
	status, err := MakeGenesisStatus(genDoc)
	if err != nil {
		return nil, errors.Wrap(err, "failed to make genesis status")
	}
	//blockStore := bc.NewBlockStore(blockStoreDB)

	// TODO hucc

	eventBus := types.NewEventBus()
	eventBus.SetLogger(logger.With("module", "events"))
	if err := eventBus.Start(); err != nil {
		return nil, errors.Wrap(err, "failed to start event bus")
	}
	defer eventBus.Stop()
	mempool := MockMempool{}
	evpool := MockEvidencePool{}
	blockExec := NewBlockExecutor(statusDB, log.Test(), evpool)
	consensusState := NewConsensusState(config.Consensus, status.Copy(), blockExec, nil, mempool, evpool)
	consensusState.SetLogger(logger)
	consensusState.SetEventBus(eventBus)
	if privValidator != nil {
		consensusState.SetPrivValidator(privValidator)
	}
	// END OF COPY PASTE
	/////////////////////////////////////////////////////////////////////////////

	// set consensus wal to buffered WAL, which will write all incoming msgs to buffer
	var b bytes.Buffer
	wr := bufio.NewWriter(&b)
	numBlocksWritten := make(chan struct{})
	wal := newByteBufferWAL(logger, NewWALEncoder(wr), numBlocks, numBlocksWritten)
	// see wal.go#103
	wal.Write(EndHeightMessage{0})
	consensusState.wal = wal

	if err := consensusState.Start(); err != nil {
		return nil, errors.Wrap(err, "failed to start consensus state")
	}

	select {
	case <-numBlocksWritten:
		consensusState.Stop()
		wr.Flush()
		return b.Bytes(), nil
	case <-time.After(1 * time.Minute):
		consensusState.Stop()
		return []byte{}, fmt.Errorf("waited too long to produce %d blocks (grep logs for `wal_generator`)", numBlocks)
	}
}

// f**ing long, but unique for each test
func makePathname() string {
	// get path
	p, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	// fmt.Println(p)
	sep := string(filepath.Separator)
	return strings.Replace(p, sep, "_", -1)
}

func randPort() int {
	// returns between base and base + spread
	base, spread := 20000, 20000
	return base + cmn.RandIntn(spread)
}

func makeAddrs() (string, string) {
	start := randPort()
	return fmt.Sprintf("tcp://0.0.0.0:%d", start),
		fmt.Sprintf("tcp://0.0.0.0:%d", start+1)
}

// getConfig returns a config for test cases
func getConfig() *cfg.Config {
	pathname := makePathname()
	c := cfg.ResetTestRoot(fmt.Sprintf("%s_%d", pathname, cmn.RandInt()))

	// and we use random ports to run in parallel
	tm, rpc := makeAddrs()
	c.P2P.ListenAddress = tm
	c.RPC.HTTPEndpoint = rpc
	return c
}

// byteBufferWAL is a WAL which writes all msgs to a byte buffer. Writing stops
// when the heightToStop is reached. Client will be notified via
// signalWhenStopsTo channel.
type byteBufferWAL struct {
	enc               *WALEncoder
	stopped           bool
	heightToStop      uint64
	signalWhenStopsTo chan<- struct{}

	logger log.Logger
}

// needed for determinism
var fixedTime, _ = time.Parse(time.RFC3339, "2017-01-02T15:04:05Z")

func newByteBufferWAL(logger log.Logger, enc *WALEncoder, nBlocks uint64, signalStop chan<- struct{}) *byteBufferWAL {
	return &byteBufferWAL{
		enc:               enc,
		heightToStop:      nBlocks,
		signalWhenStopsTo: signalStop,
		logger:            logger,
	}
}

// Save writes message to the internal buffer except when heightToStop is
// reached, in which case it will signal the caller via signalWhenStopsTo and
// skip writing.
func (w *byteBufferWAL) Write(m WALMessage) {
	if w.stopped {
		w.logger.Debug("WAL already stopped. Not writing message", "msg", m)
		return
	}

	if endMsg, ok := m.(EndHeightMessage); ok {
		w.logger.Debug("WAL write end height message", "height", endMsg.Height, "stopHeight", w.heightToStop)
		if endMsg.Height == w.heightToStop {
			w.logger.Debug("Stopping WAL at height", "height", endMsg.Height)
			w.signalWhenStopsTo <- struct{}{}
			w.stopped = true
			return
		}
	}

	w.logger.Debug("WAL Write Message", "msg", m)
	err := w.enc.Encode(&TimedWALMessage{fixedTime, m})
	if err != nil {
		panic(fmt.Sprintf("failed to encode the msg %v", m))
	}
}

func (w *byteBufferWAL) WriteSync(m WALMessage) {
	w.Write(m)
}

func (w *byteBufferWAL) Group() *auto.Group {
	panic("not implemented")
}
func (w *byteBufferWAL) SearchForEndHeight(height uint64, options *WALSearchOptions) (gr *auto.GroupReader, found bool, err error) {
	return nil, false, nil
}

func (w *byteBufferWAL) Start() error { return nil }
func (w *byteBufferWAL) Stop() error  { return nil }
func (w *byteBufferWAL) Wait()        {}
