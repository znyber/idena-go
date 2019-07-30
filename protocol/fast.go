package protocol

import (
	"fmt"
	"github.com/deckarep/golang-set"
	"github.com/idena-network/idena-go/blockchain"
	"github.com/idena-network/idena-go/blockchain/types"
	"github.com/idena-network/idena-go/common/eventbus"
	"github.com/idena-network/idena-go/core/appstate"
	"github.com/idena-network/idena-go/core/state"
	"github.com/idena-network/idena-go/core/state/snapshot"
	"github.com/idena-network/idena-go/core/validators"
	"github.com/idena-network/idena-go/events"
	"github.com/idena-network/idena-go/ipfs"
	"github.com/idena-network/idena-go/log"
	"github.com/pkg/errors"
	"os"
	"time"
)

const FastSyncBatchSize = 1000

type fastSync struct {
	pm                   *ProtocolManager
	log                  log.Logger
	chain                *blockchain.Blockchain
	batches              chan *batch
	ipfs                 ipfs.Proxy
	isSyncing            bool
	appState             *appstate.AppState
	potentialForkedPeers mapset.Set
	manifest             *snapshot.Manifest
	stateDb              *state.IdentityStateDB
	validators           *validators.ValidatorsCache
	sm                   *state.SnapshotManager
	bus                  eventbus.Bus
}

func (fs *fastSync) batchSize() uint64 {
	return FastSyncBatchSize
}

func NewFastSync(pm *ProtocolManager, log log.Logger,
	chain *blockchain.Blockchain,
	ipfs ipfs.Proxy,
	appState *appstate.AppState,
	potentialForkedPeers mapset.Set,
	manifest *snapshot.Manifest, sm *state.SnapshotManager, bus eventbus.Bus) *fastSync {

	return &fastSync{
		appState:             appState,
		log:                  log,
		potentialForkedPeers: potentialForkedPeers,
		chain:                chain,
		batches:              make(chan *batch, 10),
		pm:                   pm,
		isSyncing:            true,
		ipfs:                 ipfs,
		manifest:             manifest,
		sm:                   sm,
		bus:                  bus,
	}
}

func (fs *fastSync) createPreliminaryCopy(height uint64) (*state.IdentityStateDB, error) {
	return fs.appState.IdentityState.CreatePreliminaryCopy(height)
}

func (fs *fastSync) dropPreliminaries() {
	fs.chain.RemovePreliminaryHead()
	fs.stateDb.DropPreliminary()
	fs.stateDb = nil
}

func (fs *fastSync) loadValidators() {
	fs.validators = validators.NewValidatorsCache(fs.stateDb, fs.appState.State.GodAddress())
	fs.validators.Load()
}

func (fs *fastSync) preConsuming(head *types.Header) (from uint64, err error) {
	if fs.chain.PreliminaryHead == nil {
		fs.chain.PreliminaryHead = head
		fs.stateDb, err = fs.createPreliminaryCopy(head.Height())
		from = head.Height() + 1
		fs.loadValidators()
		return from, err
	}
	fs.stateDb, err = fs.appState.IdentityState.LoadPreliminary(fs.chain.PreliminaryHead.Height())
	if err != nil {
		fs.dropPreliminaries()
		return fs.preConsuming(head)
	}
	fs.loadValidators()
	from = fs.chain.PreliminaryHead.Height() + 1
	return from, nil
}

func (fs *fastSync) processBatch(batch *batch, attemptNum int) error {
	if fs.manifest == nil {
		panic("manifest is required")
	}
	fs.log.Info("Start process batch", "from", batch.from, "to", batch.to)
	if attemptNum > MaxAttemptsCountPerBatch {
		return errors.New("number of attempts exceeded limit")
	}
	reload := func(from uint64) error {
		b := fs.requestBatch(from, batch.to, batch.p.id)
		if b == nil {
			return errors.New(fmt.Sprintf("batch (%v-%v) can't be loaded", from, batch.to))
		}
		return fs.processBatch(b, attemptNum+1)
	}

	for i := batch.from; i <= batch.to; i++ {
		timeout := time.After(time.Second * 10)

		select {
		case block := <-batch.headers:
			if err := fs.validateHeader(block); err != nil {
				if err == blockchain.ParentHashIsInvalid {
					fs.potentialForkedPeers.Add(batch.p.id)
					return err
				}
				fs.log.Error("Block header is invalid", "err", err)
				return reload(i)
			}
			if err := fs.validateIdentityState(block); err != nil {
				return err
			}
			fs.stateDb.CommitTree()
			if fs.chain.AddHeader(block.Header) != nil {
				return reload(i)
			}
			if !block.IdentityDiff.Empty() {
				fs.loadValidators()
			}
			fs.chain.WriteIdentityStateDiff(block.Header.Height(), block.IdentityDiff)
			if !block.Cert.Empty() {
				fs.chain.WriteCertificate(block.Header.Hash(), block.Cert, true)
			}


		case <-timeout:
			fs.log.Warn("process batch - timeout was reached")
			return reload(i)
		}
	}
	fs.log.Info("Finish process batch", "from", batch.from, "to", batch.to)
	return nil
}

func (fs *fastSync) validateIdentityState(block *block) error {
	fs.stateDb.AddDiff(block.IdentityDiff)
	if fs.stateDb.Root() != block.Header.IdentityRoot() {
		fs.stateDb.Reset()
		return errors.New("identity root is invalid")
	}
	return nil
}

func (fs *fastSync) validateHeader(block *block) error {
	prevBlock := fs.chain.PreliminaryHead

	err := fs.chain.ValidateHeader(block.Header, prevBlock)
	if err != nil {
		return err
	}

	if block.Header.Flags().HasFlag(types.IdentityUpdate) {
		if block.Cert.Empty() {
			return BlockCertIsMissing
		}
	}
	if !block.Cert.Empty() {
		return fs.chain.ValidateBlockCert(prevBlock, block.Header, block.Cert, fs.validators)
	}

	return nil
}

func (fs *fastSync) requestBatch(from, to uint64, ignoredPeer string) *batch {
	knownHeights := fs.pm.GetKnownHeights()
	if knownHeights == nil {
		return nil
	}
	for peerId, height := range knownHeights {
		if (peerId != ignoredPeer || len(knownHeights) == 1) && height >= to {
			if err, batch := fs.pm.GetBlocksRange(peerId, from, to); err != nil {
				continue
			} else {
				return batch
			}
		}
	}
	return nil
}

func (fs *fastSync) postConsuming() error {
	if fs.chain.PreliminaryHead.Height() != fs.manifest.Height {
		return errors.New("preliminary head is lower than manifest's head")
	}

	if fs.chain.PreliminaryHead.Root() != fs.manifest.Root {
		return errors.New("preliminary head's root doesn't equal manifest's root")
	}
	fs.log.Info("Start loading of snapshot", "height", fs.manifest.Height)
	filePath, err := fs.sm.DownloadSnapshot(fs.manifest)
	if err != nil {
		return errors.WithMessage(err, "snapshot's downloading has been failed")
	}
	fs.log.Info("Snapshot has been loaded", "height", fs.manifest.Height)

	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	err = fs.appState.State.RecoverSnapshot(fs.manifest, file)
	if err != nil {
		return err
	}

	err = fs.appState.IdentityState.SwitchToPreliminary(fs.manifest.Height)
	if err != nil {
		return err
	}
	fs.appState.ValidatorsCache.Load()
	fs.chain.SwitchToPreliminary()
	fs.chain.RemovePreliminaryHead()
	fs.bus.Publish(events.FastSyncCompletedEvent{})
	return nil
}