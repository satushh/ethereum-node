// Package enginebridge implements prysm's execution-engine call overrides on
// geth's catalyst.ConsensusAPI: typed in-process engine calls with the blobs
// bundle handed over by reference, no JSON round trip. It consumes the
// WithGetPayloadOverride / WithGetBlobsOverride seam carried on the sibling
// prysm branch engine-override-seam (so building this package requires the
// go.work overlay; see the README's engine transport ladder).
//
// Conversions mirror prysm's own JSON decoders byte for byte (little-endian
// 32-byte fee/value fields, padding, empty-slice conventions; see
// (*pb.ExecutionBundleFulu).UnmarshalJSON) with two deliberate exceptions:
// blobs, commitments, and transactions alias geth-owned memory instead of
// being copied out of a hex string. Geth never mutates a built payload's
// sidecar data in place, and prysm holds the bundle only through the
// proposal flow, so the aliases are sound and the ~128 KiB-per-blob copies
// disappear along with the serialization.
package enginebridge

import (
	"context"
	"fmt"
	"math/big"

	beaconexecution "github.com/OffchainLabs/prysm/v7/beacon-chain/execution"
	fieldparams "github.com/OffchainLabs/prysm/v7/config/fieldparams"
	"github.com/OffchainLabs/prysm/v7/consensus-types/primitives"
	"github.com/OffchainLabs/prysm/v7/encoding/bytesutil"
	pb "github.com/OffchainLabs/prysm/v7/proto/engine/v1"
	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/catalyst"
	"google.golang.org/protobuf/proto"
)

// GetPayloadOverride returns a typed engine_getPayloadV5 implementation on
// the given catalyst instance. The instance must be the same one serving the
// engine JSON-RPC namespace: payload IDs minted by forkchoiceUpdated live in
// a per-instance queue.
func GetPayloadOverride(api *catalyst.ConsensusAPI) beaconexecution.GetPayloadOverride {
	return func(_ context.Context, payloadId [8]byte, _ primitives.Slot, method string, result proto.Message) error {
		if method != beaconexecution.GetPayloadMethodV5 {
			return fmt.Errorf("enginebridge: no typed implementation for %s (only %s)", method, beaconexecution.GetPayloadMethodV5)
		}
		bundle, ok := result.(*pb.ExecutionBundleFulu)
		if !ok {
			return fmt.Errorf("enginebridge: %s expects *pb.ExecutionBundleFulu, got %T", method, result)
		}
		env, err := api.GetPayloadV5(engine.PayloadID(payloadId))
		if err != nil {
			return fmt.Errorf("typed %s: %w", method, err)
		}
		return fillBundleFulu(bundle, env)
	}
}

// GetBlobsOverride returns a typed engine_getBlobsV2/V3 implementation on
// the given catalyst instance. Inert on a devnet without peers (the call
// only fires for gossip-received blocks) but completes the blob path.
func GetBlobsOverride(api *catalyst.ConsensusAPI) beaconexecution.GetBlobsOverride {
	return func(ctx context.Context, method string, versionedHashes []common.Hash) ([]*pb.BlobAndProofV2, error) {
		var list engine.BlobAndProofListV2
		var err error
		switch method {
		case beaconexecution.GetBlobsV2:
			list, err = api.GetBlobsV2(ctx, versionedHashes)
		case beaconexecution.GetBlobsV3:
			list, err = api.GetBlobsV3(ctx, versionedHashes)
		default:
			return nil, fmt.Errorf("enginebridge: no typed implementation for %s", method)
		}
		if err != nil {
			return nil, fmt.Errorf("typed %s: %w", method, err)
		}
		if list == nil {
			// all-or-nothing miss (V2) or pre-Osaka: the JSON path would
			// decode the null response to a nil slice too
			return nil, nil
		}
		out := make([]*pb.BlobAndProofV2, len(list))
		for i, bp := range list {
			if bp == nil {
				continue // V3 partial response: holes stay nil
			}
			out[i] = &pb.BlobAndProofV2{
				Blob:      bp.Blob,
				KzgProofs: pb.RecastHexutilByteSlice(bp.CellProofs),
			}
		}
		return out, nil
	}
}

// fillBundleFulu populates the prysm getPayloadV5 response proto from geth's
// payload envelope, field-for-field equivalent to prysm's JSON decoder.
func fillBundleFulu(out *pb.ExecutionBundleFulu, env *engine.ExecutionPayloadEnvelope) error {
	d := env.ExecutionPayload
	if d == nil {
		return fmt.Errorf("enginebridge: payload envelope without execution payload")
	}
	if d.BaseFeePerGas == nil || env.BlockValue == nil {
		return fmt.Errorf("enginebridge: payload %#x lacks base fee or block value", d.BlockHash)
	}
	if d.BlobGasUsed == nil || d.ExcessBlobGas == nil {
		return fmt.Errorf("enginebridge: payload %#x lacks blob gas fields (pre-Cancun payload for a Fulu slot?)", d.BlockHash)
	}
	out.Payload = &pb.ExecutionPayloadDeneb{
		ParentHash:    d.ParentHash.Bytes(),
		FeeRecipient:  d.FeeRecipient.Bytes(),
		StateRoot:     d.StateRoot.Bytes(),
		ReceiptsRoot:  d.ReceiptsRoot.Bytes(),
		LogsBloom:     d.LogsBloom,
		PrevRandao:    d.Random.Bytes(),
		BlockNumber:   d.Number,
		GasLimit:      d.GasLimit,
		GasUsed:       d.GasUsed,
		Timestamp:     d.Timestamp,
		ExtraData:     d.ExtraData,
		BaseFeePerGas: littleEndian32(d.BaseFeePerGas),
		BlockHash:     d.BlockHash.Bytes(),
		Transactions:  d.Transactions,
		Withdrawals:   convertWithdrawals(d.Withdrawals),
		BlobGasUsed:   *d.BlobGasUsed,
		ExcessBlobGas: *d.ExcessBlobGas,
	}
	out.Value = littleEndian32(env.BlockValue)
	out.ShouldOverrideBuilder = env.Override
	if env.Requests == nil {
		out.ExecutionRequests = [][]byte{}
	} else {
		out.ExecutionRequests = env.Requests
	}
	b := env.BlobsBundle
	if b == nil {
		// geth's V5 envelope always carries a bundle; mirror the JSON
		// decoder's shape anyway rather than leave downstream a nil
		out.BlobsBundle = &pb.BlobsBundleV2{KzgCommitments: [][]byte{}, Proofs: [][]byte{}, Blobs: [][]byte{}}
		return nil
	}
	out.BlobsBundle = &pb.BlobsBundleV2{
		KzgCommitments: pb.RecastHexutilByteSlice(b.Commitments),
		Proofs:         pb.RecastHexutilByteSlice(b.Proofs), // flat cell proofs, 128 per blob, same layout both sides
		Blobs:          pb.RecastHexutilByteSlice(b.Blobs),  // by reference: the copies this transport exists to remove
	}
	return nil
}

// littleEndian32 converts a big.Int to the 32-byte little-endian form
// prysm's protos use for fee and value fields.
func littleEndian32(v *big.Int) []byte {
	return bytesutil.PadTo(bytesutil.ReverseByteOrder(v.Bytes()), fieldparams.RootLength)
}

func convertWithdrawals(ws []*types.Withdrawal) []*pb.Withdrawal {
	out := make([]*pb.Withdrawal, len(ws))
	for i, w := range ws {
		out[i] = &pb.Withdrawal{
			Index:          w.Index,
			ValidatorIndex: primitives.ValidatorIndex(w.Validator),
			Address:        w.Address.Bytes(),
			Amount:         w.Amount,
		}
	}
	return out
}
