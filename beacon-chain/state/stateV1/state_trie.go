package stateV1

import (
	"context"
	"runtime"
	"sort"
	"sync"

	"github.com/gogo/protobuf/proto"
	"github.com/pkg/errors"
	iface "github.com/prysmaticlabs/prysm/beacon-chain/state/interface"
	"github.com/prysmaticlabs/prysm/beacon-chain/state/stateV0"
	pbp2p "github.com/prysmaticlabs/prysm/proto/beacon/p2p/v1"
	"github.com/prysmaticlabs/prysm/shared/bytesutil"
	"github.com/prysmaticlabs/prysm/shared/hashutil"
	"github.com/prysmaticlabs/prysm/shared/htrutils"
	"github.com/prysmaticlabs/prysm/shared/params"
	"github.com/prysmaticlabs/prysm/shared/sliceutil"
	"go.opencensus.io/trace"
)

// InitializeFromProto the beacon state from a protobuf representation.
func InitializeFromProto(st *pbp2p.BeaconStateV1) (*BeaconState, error) {
	return InitializeFromProtoUnsafe(proto.Clone(st).(*pbp2p.BeaconStateV1))
}

// InitializeFromProtoUnsafe directly uses the beacon state protobuf pointer
// and sets it as the inner state of the BeaconState type.
func InitializeFromProtoUnsafe(st *pbp2p.BeaconStateV1) (*BeaconState, error) {
	if st == nil {
		return nil, errors.New("received nil state")
	}

	fieldCount := params.BeaconConfig().BeaconStateFieldCount
	b := &BeaconState{
		state:                 st,
		dirtyFields:           make(map[fieldIndex]interface{}, fieldCount),
		dirtyIndices:          make(map[fieldIndex][]uint64, fieldCount),
		stateFieldLeaves:      make(map[fieldIndex]*FieldTrie, fieldCount),
		sharedFieldReferences: make(map[fieldIndex]*reference, 10),
		rebuildTrie:           make(map[fieldIndex]bool, fieldCount),
		valMapHandler:         newValHandler(st.Validators),
	}

	for i := 0; i < fieldCount; i++ {
		b.dirtyFields[fieldIndex(i)] = true
		b.rebuildTrie[fieldIndex(i)] = true
		b.dirtyIndices[fieldIndex(i)] = []uint64{}
		b.stateFieldLeaves[fieldIndex(i)] = &FieldTrie{
			field:     fieldIndex(i),
			reference: &reference{refs: 1},
			Mutex:     new(sync.Mutex),
		}
	}

	// Initialize field reference tracking for shared data.
	b.sharedFieldReferences[randaoMixes] = &reference{refs: 1}
	b.sharedFieldReferences[stateRoots] = &reference{refs: 1}
	b.sharedFieldReferences[blockRoots] = &reference{refs: 1}
	b.sharedFieldReferences[previousEpochParticipationBits] = &reference{refs: 1}
	b.sharedFieldReferences[currentEpochParticipationBits] = &reference{refs: 1}
	b.sharedFieldReferences[slashings] = &reference{refs: 1}
	b.sharedFieldReferences[eth1DataVotes] = &reference{refs: 1}
	b.sharedFieldReferences[validators] = &reference{refs: 1}
	b.sharedFieldReferences[balances] = &reference{refs: 1}
	b.sharedFieldReferences[historicalRoots] = &reference{refs: 1}

	return b, nil
}

// Copy returns a deep copy of the beacon state.
func (b *BeaconState) Copy() iface.BeaconState {
	if !b.HasInnerState() {
		return nil
	}

	b.lock.RLock()
	defer b.lock.RUnlock()
	fieldCount := params.BeaconConfig().BeaconStateFieldCount
	dst := &BeaconState{
		state: &pbp2p.BeaconStateV1{
			// Primitive types, safe to copy.
			GenesisTime:      b.state.GenesisTime,
			Slot:             b.state.Slot,
			Eth1DepositIndex: b.state.Eth1DepositIndex,

			// Large arrays, infrequently changed, constant size.
			RandaoMixes:   b.state.RandaoMixes,
			StateRoots:    b.state.StateRoots,
			BlockRoots:    b.state.BlockRoots,
			Slashings:     b.state.Slashings,
			Eth1DataVotes: b.state.Eth1DataVotes,

			// Large arrays, increases over time.
			Validators:                 b.state.Validators,
			Balances:                   b.state.Balances,
			HistoricalRoots:            b.state.HistoricalRoots,
			PreviousEpochParticipation: b.state.PreviousEpochParticipation,
			CurrentEpochParticipation:  b.state.CurrentEpochParticipation,

			// Everything else, too small to be concerned about, constant size.
			Fork:                        b.fork(),
			LatestBlockHeader:           b.latestBlockHeader(),
			Eth1Data:                    b.eth1Data(),
			JustificationBits:           b.justificationBits(),
			PreviousJustifiedCheckpoint: b.previousJustifiedCheckpoint(),
			CurrentJustifiedCheckpoint:  b.currentJustifiedCheckpoint(),
			FinalizedCheckpoint:         b.finalizedCheckpoint(),
			GenesisValidatorsRoot:       b.genesisValidatorRoot(),
			CurrentSyncCommittee:        b.CurrentSyncCommittee(),
			NextSyncCommittee:           b.NextSyncCommittee(),
		},
		dirtyFields:           make(map[fieldIndex]interface{}, fieldCount),
		dirtyIndices:          make(map[fieldIndex][]uint64, fieldCount),
		rebuildTrie:           make(map[fieldIndex]bool, fieldCount),
		sharedFieldReferences: make(map[fieldIndex]*reference, 10),
		stateFieldLeaves:      make(map[fieldIndex]*FieldTrie, fieldCount),

		// Copy on write validator index map.
		valMapHandler: b.valMapHandler,
	}

	for field, ref := range b.sharedFieldReferences {
		ref.AddRef()
		dst.sharedFieldReferences[field] = ref
	}

	// Increment ref for validator map
	b.valMapHandler.mapRef.AddRef()

	for i := range b.dirtyFields {
		dst.dirtyFields[i] = true
	}

	for i := range b.dirtyIndices {
		indices := make([]uint64, len(b.dirtyIndices[i]))
		copy(indices, b.dirtyIndices[i])
		dst.dirtyIndices[i] = indices
	}

	for i := range b.rebuildTrie {
		dst.rebuildTrie[i] = true
	}

	for fldIdx, fieldTrie := range b.stateFieldLeaves {
		dst.stateFieldLeaves[fldIdx] = fieldTrie
		if fieldTrie.reference != nil {
			fieldTrie.Lock()
			fieldTrie.AddRef()
			fieldTrie.Unlock()
		}
	}

	if b.merkleLayers != nil {
		dst.merkleLayers = make([][][]byte, len(b.merkleLayers))
		for i, layer := range b.merkleLayers {
			dst.merkleLayers[i] = make([][]byte, len(layer))
			for j, content := range layer {
				dst.merkleLayers[i][j] = make([]byte, len(content))
				copy(dst.merkleLayers[i][j], content)
			}
		}
	}

	// Finalizer runs when dst is being destroyed in garbage collection.
	runtime.SetFinalizer(dst, func(b *BeaconState) {
		for field, v := range b.sharedFieldReferences {
			v.MinusRef()
			if b.stateFieldLeaves[field].reference != nil {
				b.stateFieldLeaves[field].MinusRef()
			}
		}
	})

	return dst
}

// HashTreeRoot of the beacon state retrieves the Merkle root of the trie
// representation of the beacon state based on the eth2 Simple Serialize specification.
func (b *BeaconState) HashTreeRoot(ctx context.Context) ([32]byte, error) {
	_, span := trace.StartSpan(ctx, "beaconState.HashTreeRoot")
	defer span.End()

	b.lock.Lock()
	defer b.lock.Unlock()

	if b.merkleLayers == nil || len(b.merkleLayers) == 0 {
		// TODO: Needs its own compute field roots
		fieldRoots, err := computeFieldRoots(b.state)
		if err != nil {
			return [32]byte{}, err
		}
		layers := merkleize(fieldRoots)
		b.merkleLayers = layers
		b.dirtyFields = make(map[fieldIndex]interface{}, params.BeaconConfig().BeaconStateFieldCount)
	}

	for field := range b.dirtyFields {
		root, err := b.rootSelector(field)
		if err != nil {
			return [32]byte{}, err
		}
		b.merkleLayers[0][field] = root[:]
		b.recomputeRoot(int(field))
		delete(b.dirtyFields, field)
	}
	return bytesutil.ToBytes32(b.merkleLayers[len(b.merkleLayers)-1][0]), nil
}

// FieldReferencesCount returns the reference count held by each field. This
// also includes the field trie held by each field.
func (b *BeaconState) FieldReferencesCount() map[string]uint64 {
	refMap := make(map[string]uint64)
	b.lock.RLock()
	defer b.lock.RUnlock()
	for i, f := range b.sharedFieldReferences {
		refMap[i.String()] = uint64(f.Refs())
	}
	for i, f := range b.stateFieldLeaves {
		numOfRefs := uint64(f.Refs())
		f.lock.RLock()
		if len(f.fieldLayers) != 0 {
			refMap[i.String()+"_trie"] = numOfRefs
		}
		f.lock.RUnlock()
	}
	return refMap
}

// Merkleize 32-byte leaves into a Merkle trie for its adequate depth, returning
// the resulting layers of the trie based on the appropriate depth. This function
// pads the leaves to a length of 32.
func merkleize(leaves [][]byte) [][][]byte {
	hashFunc := hashutil.CustomSHA256Hasher()
	layers := make([][][]byte, htrutils.Depth(uint64(len(leaves)))+1)
	for len(leaves) != 32 {
		leaves = append(leaves, make([]byte, 32))
	}
	currentLayer := leaves
	layers[0] = currentLayer

	// We keep track of the hash layers of a Merkle trie until we reach
	// the top layer of length 1, which contains the single root element.
	//        [Root]      -> Top layer has length 1.
	//    [E]       [F]   -> This layer has length 2.
	// [A]  [B]  [C]  [D] -> The bottom layer has length 4 (needs to be a power of two).
	i := 1
	for len(currentLayer) > 1 && i < len(layers) {
		layer := make([][]byte, 0)
		for i := 0; i < len(currentLayer); i += 2 {
			hashedChunk := hashFunc(append(currentLayer[i], currentLayer[i+1]...))
			layer = append(layer, hashedChunk[:])
		}
		currentLayer = layer
		layers[i] = currentLayer
		i++
	}
	return layers
}

func (b *BeaconState) rootSelector(field fieldIndex) ([32]byte, error) {
	hasher := hashutil.CustomSHA256Hasher()
	switch field {
	case genesisTime:
		return htrutils.Uint64Root(b.state.GenesisTime), nil
	case genesisValidatorRoot:
		return bytesutil.ToBytes32(b.state.GenesisValidatorsRoot), nil
	case slot:
		return htrutils.Uint64Root(uint64(b.state.Slot)), nil
	case eth1DepositIndex:
		return htrutils.Uint64Root(b.state.Eth1DepositIndex), nil
	case fork:
		return htrutils.ForkRoot(b.state.Fork)
	case latestBlockHeader:
		return stateV0.BlockHeaderRoot(b.state.LatestBlockHeader)
	case blockRoots:
		if b.rebuildTrie[field] {
			err := b.resetFieldTrie(field, b.state.BlockRoots, uint64(params.BeaconConfig().SlotsPerHistoricalRoot))
			if err != nil {
				return [32]byte{}, err
			}
			b.dirtyIndices[field] = []uint64{}
			delete(b.rebuildTrie, field)
			return b.stateFieldLeaves[field].TrieRoot()
		}
		return b.recomputeFieldTrie(blockRoots, b.state.BlockRoots)
	case stateRoots:
		if b.rebuildTrie[field] {
			err := b.resetFieldTrie(field, b.state.StateRoots, uint64(params.BeaconConfig().SlotsPerHistoricalRoot))
			if err != nil {
				return [32]byte{}, err
			}
			b.dirtyIndices[field] = []uint64{}
			delete(b.rebuildTrie, field)
			return b.stateFieldLeaves[field].TrieRoot()
		}
		return b.recomputeFieldTrie(stateRoots, b.state.StateRoots)
	case historicalRoots:
		return htrutils.HistoricalRootsRoot(b.state.HistoricalRoots)
	case eth1Data:
		return stateV0.Eth1Root(hasher, b.state.Eth1Data)
	case eth1DataVotes:
		if b.rebuildTrie[field] {
			err := b.resetFieldTrie(field, b.state.Eth1DataVotes, uint64(params.BeaconConfig().SlotsPerEpoch.Mul(uint64(params.BeaconConfig().EpochsPerEth1VotingPeriod))))
			if err != nil {
				return [32]byte{}, err
			}
			b.dirtyIndices[field] = []uint64{}
			delete(b.rebuildTrie, field)
			return b.stateFieldLeaves[field].TrieRoot()
		}
		return b.recomputeFieldTrie(field, b.state.Eth1DataVotes)
	case validators:
		if b.rebuildTrie[field] {
			err := b.resetFieldTrie(field, b.state.Validators, params.BeaconConfig().ValidatorRegistryLimit)
			if err != nil {
				return [32]byte{}, err
			}
			b.dirtyIndices[validators] = []uint64{}
			delete(b.rebuildTrie, validators)
			return b.stateFieldLeaves[field].TrieRoot()
		}
		return b.recomputeFieldTrie(validators, b.state.Validators)
	case balances:
		return stateV0.ValidatorBalancesRoot(b.state.Balances)
	case randaoMixes:
		if b.rebuildTrie[field] {
			err := b.resetFieldTrie(field, b.state.RandaoMixes, uint64(params.BeaconConfig().EpochsPerHistoricalVector))
			if err != nil {
				return [32]byte{}, err
			}
			b.dirtyIndices[field] = []uint64{}
			delete(b.rebuildTrie, field)
			return b.stateFieldLeaves[field].TrieRoot()
		}
		return b.recomputeFieldTrie(randaoMixes, b.state.RandaoMixes)
	case slashings:
		return htrutils.SlashingsRoot(b.state.Slashings)
	case previousEpochParticipationBits:
		return participationBitsRoot(b.state.PreviousEpochParticipation)
	case currentEpochParticipationBits:
		return participationBitsRoot(b.state.CurrentEpochParticipation)
	case justificationBits:
		return bytesutil.ToBytes32(b.state.JustificationBits), nil
	case previousJustifiedCheckpoint:
		return htrutils.CheckpointRoot(hasher, b.state.PreviousJustifiedCheckpoint)
	case currentJustifiedCheckpoint:
		return htrutils.CheckpointRoot(hasher, b.state.CurrentJustifiedCheckpoint)
	case finalizedCheckpoint:
		return htrutils.CheckpointRoot(hasher, b.state.FinalizedCheckpoint)
	case currentSyncCommittee:
		return syncCommitteeRoot(b.state.CurrentSyncCommittee)
	case nextSyncCommittee:
		return syncCommitteeRoot(b.state.NextSyncCommittee)
	}
	return [32]byte{}, errors.New("invalid field index provided")
}

func (b *BeaconState) recomputeFieldTrie(index fieldIndex, elements interface{}) ([32]byte, error) {
	fTrie := b.stateFieldLeaves[index]
	if fTrie.Refs() > 1 {
		fTrie.Lock()
		defer fTrie.Unlock()
		fTrie.MinusRef()
		newTrie := fTrie.CopyTrie()
		b.stateFieldLeaves[index] = newTrie
		fTrie = newTrie
	}
	// remove duplicate indexes
	b.dirtyIndices[index] = sliceutil.SetUint64(b.dirtyIndices[index])
	// sort indexes again
	sort.Slice(b.dirtyIndices[index], func(i int, j int) bool {
		return b.dirtyIndices[index][i] < b.dirtyIndices[index][j]
	})
	root, err := fTrie.RecomputeTrie(b.dirtyIndices[index], elements)
	if err != nil {
		return [32]byte{}, err
	}
	b.dirtyIndices[index] = []uint64{}
	return root, nil
}

func (b *BeaconState) resetFieldTrie(index fieldIndex, elements interface{}, length uint64) error {
	fTrie, err := NewFieldTrie(index, elements, length)
	if err != nil {
		return err
	}
	b.stateFieldLeaves[index] = fTrie
	b.dirtyIndices[index] = []uint64{}
	return nil
}