// Copyright (C) 2019-2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package executor

import (
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/ava-labs/avalanchego/chains/atomic"
	"github.com/ava-labs/avalanchego/database/prefixdb"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/crypto"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/avax"
	"github.com/ava-labs/avalanchego/vms/platformvm/state"
	"github.com/ava-labs/avalanchego/vms/platformvm/transactions/unsigned"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
)

func TestNewImportTx(t *testing.T) {
	h := newTestHelpersCollection()
	defer func() {
		if err := internalStateShutdown(h); err != nil {
			t.Fatal(err)
		}
	}()

	type test struct {
		description   string
		sourceChainID ids.ID
		sharedMemory  atomic.SharedMemory
		sourceKeys    []*crypto.PrivateKeySECP256K1R
		timestamp     time.Time
		shouldErr     bool
		shouldVerify  bool
	}

	factory := crypto.FactorySECP256K1R{}
	sourceKeyIntf, err := factory.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	sourceKey := sourceKeyIntf.(*crypto.PrivateKeySECP256K1R)

	cnt := new(byte)

	// Returns a shared memory where GetDatabase returns a database
	// where [recipientKey] has a balance of [amt]
	fundedSharedMemory := func(peerChain ids.ID, amt uint64) atomic.SharedMemory {
		*cnt++
		m := &atomic.Memory{}
		err := m.Initialize(logging.NoLog{}, prefixdb.New([]byte{*cnt}, h.baseDB))
		if err != nil {
			t.Fatal(err)
		}

		sm := m.NewSharedMemory(h.ctx.ChainID)
		peerSharedMemory := m.NewSharedMemory(peerChain)

		// #nosec G404
		utxo := &avax.UTXO{
			UTXOID: avax.UTXOID{
				TxID:        ids.GenerateTestID(),
				OutputIndex: rand.Uint32(),
			},
			Asset: avax.Asset{ID: h.ctx.AVAXAssetID},
			Out: &secp256k1fx.TransferOutput{
				Amt: amt,
				OutputOwners: secp256k1fx.OutputOwners{
					Locktime:  0,
					Addrs:     []ids.ShortID{sourceKey.PublicKey().Address()},
					Threshold: 1,
				},
			},
		}
		utxoBytes, err := unsigned.Codec.Marshal(unsigned.Version, utxo)
		if err != nil {
			t.Fatal(err)
		}
		inputID := utxo.InputID()
		if err := peerSharedMemory.Apply(map[ids.ID]*atomic.Requests{h.ctx.ChainID: {PutRequests: []*atomic.Element{{
			Key:   inputID[:],
			Value: utxoBytes,
			Traits: [][]byte{
				sourceKey.PublicKey().Address().Bytes(),
			},
		}}}}); err != nil {
			t.Fatal(err)
		}

		return sm
	}

	tests := []test{
		{
			description:   "can't pay fee",
			sourceChainID: h.ctx.XChainID,
			sharedMemory:  fundedSharedMemory(h.ctx.XChainID, h.cfg.TxFee-1),
			sourceKeys:    []*crypto.PrivateKeySECP256K1R{sourceKey},
			shouldErr:     true,
		},
		{
			description:   "can barely pay fee",
			sourceChainID: h.ctx.XChainID,
			sharedMemory:  fundedSharedMemory(h.ctx.XChainID, h.cfg.TxFee),
			sourceKeys:    []*crypto.PrivateKeySECP256K1R{sourceKey},
			shouldErr:     false,
			shouldVerify:  true,
		},
		{
			description:   "attempting to import from C-chain",
			sourceChainID: cChainID,
			sharedMemory:  fundedSharedMemory(cChainID, h.cfg.TxFee),
			sourceKeys:    []*crypto.PrivateKeySECP256K1R{sourceKey},
			timestamp:     h.cfg.ApricotPhase5Time,
			shouldErr:     false,
			shouldVerify:  true,
		},
	}

	to := ids.GenerateTestShortID()
	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			assert := assert.New(t)

			h.ctx.SharedMemory = tt.sharedMemory
			h.atomicUtxosMan = avax.NewAtomicUTXOManager(tt.sharedMemory, unsigned.Codec)
			h.txBuilder.ResetAtomicUTXOManager(h.atomicUtxosMan)
			tx, err := h.txBuilder.NewImportTx(
				tt.sourceChainID,
				to,
				tt.sourceKeys,
				ids.ShortEmpty,
			)
			if tt.shouldErr {
				assert.Error(err)
				return
			}
			assert.NoError(err)

			unsignedTx := tx.Unsigned.(*unsigned.ImportTx)
			assert.NotEmpty(unsignedTx.ImportedInputs)
			assert.Equal(len(tx.Creds), len(unsignedTx.Ins)+len(unsignedTx.ImportedInputs), "should have the same number of credentials as inputs")

			totalIn := uint64(0)
			for _, in := range unsignedTx.Ins {
				totalIn += in.Input().Amount()
			}
			for _, in := range unsignedTx.ImportedInputs {
				totalIn += in.Input().Amount()
			}
			totalOut := uint64(0)
			for _, out := range unsignedTx.Outs {
				totalOut += out.Out.Amount()
			}

			assert.Equal(h.cfg.TxFee, totalIn-totalOut, "burned too much")

			preferredState := h.tState
			fakedState := state.NewVersioned(
				preferredState,
				preferredState.CurrentStakerChainState(),
				preferredState.PendingStakerChainState(),
			)
			fakedState.SetTimestamp(tt.timestamp)

			err = h.txExecutor.SemanticVerify(tx, fakedState)
			if tt.shouldVerify {
				assert.NoError(err)
			} else {
				assert.Error(err)
			}
		})
	}
}