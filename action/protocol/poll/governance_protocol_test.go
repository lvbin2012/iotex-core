// Copyright (c) 2019 IoTeX Foundation
// This is an alpha (internal) release and is not suitable for production. This source code is provided 'as is' and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package poll

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"

	"github.com/iotexproject/go-pkgs/hash"
	"github.com/iotexproject/iotex-address/address"
	"github.com/iotexproject/iotex-election/test/mock/mock_committee"
	"github.com/iotexproject/iotex-election/types"

	"github.com/iotexproject/iotex-core/action"
	"github.com/iotexproject/iotex-core/action/protocol"
	"github.com/iotexproject/iotex-core/action/protocol/rolldpos"
	"github.com/iotexproject/iotex-core/action/protocol/vote"
	"github.com/iotexproject/iotex-core/action/protocol/vote/candidatesutil"
	"github.com/iotexproject/iotex-core/config"
	"github.com/iotexproject/iotex-core/db"
	"github.com/iotexproject/iotex-core/db/batch"
	"github.com/iotexproject/iotex-core/state"
	"github.com/iotexproject/iotex-core/test/identityset"
	"github.com/iotexproject/iotex-core/test/mock/mock_chainmanager"
)

func initConstruct(ctrl *gomock.Controller) (Protocol, context.Context, protocol.StateManager, *types.ElectionResult, error) {
	cfg := config.Default
	cfg.Genesis.EasterBlockHeight = 1 // set up testing after Easter Height
	cfg.Genesis.KickoutIntensityRate = 90
	cfg.Genesis.KickoutEpochPeriod = 2
	cfg.Genesis.ProductivityThreshold = 85
	ctx := protocol.WithBlockCtx(
		context.Background(),
		protocol.BlockCtx{
			BlockHeight: 0,
		},
	)
	registry := protocol.NewRegistry()
	rp := rolldpos.NewProtocol(36, 36, 20)
	err := registry.Register("rolldpos", rp)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	epochStartHeight := rp.GetEpochHeight(2)
	ctx = protocol.WithBlockchainCtx(
		ctx,
		protocol.BlockchainCtx{
			Genesis:  cfg.Genesis,
			Registry: registry,
			Tip: protocol.TipInfo{
				Height: epochStartHeight - 1,
			},
		},
	)
	ctx = protocol.WithActionCtx(
		ctx,
		protocol.ActionCtx{},
	)

	sm := mock_chainmanager.NewMockStateManager(ctrl)
	committee := mock_committee.NewMockCommittee(ctrl)
	cb := batch.NewCachedBatch()
	sm.EXPECT().State(gomock.Any(), gomock.Any()).DoAndReturn(
		func(account interface{}, opts ...protocol.StateOption) (uint64, error) {
			cfg, err := protocol.CreateStateConfig(opts...)
			if err != nil {
				return 0, err
			}
			val, err := cb.Get(cfg.Namespace, cfg.Key)
			if err != nil {
				return 0, state.ErrStateNotExist
			}
			return 0, state.Deserialize(account, val)
		}).AnyTimes()
	sm.EXPECT().PutState(gomock.Any(), gomock.Any()).DoAndReturn(
		func(account interface{}, opts ...protocol.StateOption) (uint64, error) {
			cfg, err := protocol.CreateStateConfig(opts...)
			if err != nil {
				return 0, err
			}
			ss, err := state.Serialize(account)
			if err != nil {
				return 0, err
			}
			cb.Put(cfg.Namespace, cfg.Key, ss, "failed to put state")
			return 0, nil
		}).AnyTimes()
	sm.EXPECT().Snapshot().Return(1).AnyTimes()
	sm.EXPECT().Height().Return(epochStartHeight-1, nil).AnyTimes()
	r := types.NewElectionResultForTest(time.Now())
	committee.EXPECT().ResultByHeight(uint64(123456)).Return(r, nil).AnyTimes()
	committee.EXPECT().HeightByTime(gomock.Any()).Return(uint64(123456), nil).AnyTimes()
	candidates := []*state.Candidate{
		{
			Address:       identityset.Address(1).String(),
			Votes:         big.NewInt(30),
			RewardAddress: "rewardAddress1",
		},
		{
			Address:       identityset.Address(2).String(),
			Votes:         big.NewInt(22),
			RewardAddress: "rewardAddress2",
		},
		{
			Address:       identityset.Address(3).String(),
			Votes:         big.NewInt(20),
			RewardAddress: "rewardAddress3",
		},
		{
			Address:       identityset.Address(4).String(),
			Votes:         big.NewInt(10),
			RewardAddress: "rewardAddress4",
		},
	}
	indexer, err := NewCandidateIndexer(db.NewMemKVStore())
	if err != nil {
		return nil, nil, nil, nil, err
	}
	p, err := NewGovernanceChainCommitteeProtocol(
		indexer,
		func(protocol.StateReader, uint64) ([]*state.Candidate, error) { return candidates, nil },
		func(protocol.StateReader, bool, ...protocol.StateOption) ([]*state.Candidate, uint64, error) {
			return candidates, 720, nil
		},
		candidatesutil.KickoutListFromDB,
		candidatesutil.UnproductiveDelegateFromDB,
		committee,
		uint64(123456),
		func(uint64) (time.Time, error) { return time.Now(), nil },
		2,
		2,
		cfg.Chain.PollInitialCandidatesInterval,
		sm,
		func(ctx context.Context, epochNum uint64) (uint64, map[string]uint64, error) {
			switch epochNum {
			case 1:
				return uint64(16),
					map[string]uint64{ // [A, B, C]
						identityset.Address(1).String(): 1, // underperformance
						identityset.Address(2).String(): 1, // underperformance
						identityset.Address(3).String(): 1, // underperformance
						identityset.Address(4).String(): 13,
					}, nil
			case 2:
				return uint64(12),
					map[string]uint64{ // [B, D]
						identityset.Address(1).String(): 7,
						identityset.Address(2).String(): 1, // underperformance
						identityset.Address(3).String(): 3,
						identityset.Address(4).String(): 1, // underperformance
					}, nil
			case 3:
				return uint64(12),
					map[string]uint64{ // [E, F]
						identityset.Address(1).String(): 5,
						identityset.Address(2).String(): 5,
						identityset.Address(5).String(): 1, // underperformance
						identityset.Address(6).String(): 1, // underperformance
					}, nil
			default:
				return 0, nil, nil
			}
		},
		cfg.Genesis.ProductivityThreshold,
		cfg.Genesis.KickoutEpochPeriod,
		cfg.Genesis.KickoutIntensityRate,
		cfg.Genesis.UnproductiveDelegateMaxCacheSize,
	)

	if err := setCandidates(ctx, sm, indexer, candidates, 1); err != nil {
		return nil, nil, nil, nil, err
	}
	return p, ctx, sm, r, err
}

func TestCreateGenesisStates(t *testing.T) {
	require := require.New(t)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	p, ctx, sm, r, err := initConstruct(ctrl)
	require.NoError(err)
	require.NoError(p.CreateGenesisStates(ctx, sm))
	var sc state.CandidateList
	candKey := candidatesutil.ConstructKey(candidatesutil.NxtCandidateKey)
	_, err = sm.State(&sc, protocol.KeyOption(candKey[:]), protocol.NamespaceOption(protocol.SystemNamespace))
	require.NoError(err)
	candidates, err := state.CandidatesToMap(sc)
	require.NoError(err)
	require.Equal(2, len(candidates))
	for _, d := range r.Delegates() {
		operator := string(d.OperatorAddress())
		addr, err := address.FromString(operator)
		require.NoError(err)
		c, ok := candidates[hash.BytesToHash160(addr.Bytes())]
		require.True(ok)
		require.Equal(addr.String(), c.Address)
	}
}

func TestCreatePostSystemActions(t *testing.T) {
	require := require.New(t)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	p, ctx, _, r, err := initConstruct(ctrl)
	require.NoError(err)
	psac, ok := p.(protocol.PostSystemActionsCreator)
	require.True(ok)
	elp, err := psac.CreatePostSystemActions(ctx)
	require.NoError(err)
	require.Equal(1, len(elp))
	act, ok := elp[0].Action().(*action.PutPollResult)
	require.True(ok)
	require.Equal(uint64(1), act.Height())
	require.Equal(uint64(0), act.AbstractAction.Nonce())
	delegates := r.Delegates()
	require.Equal(len(act.Candidates()), len(delegates))
	for _, can := range act.Candidates() {
		d := r.DelegateByName(can.CanName)
		require.NotNil(d)
	}
}

func TestCreatePreStates(t *testing.T) {
	require := require.New(t)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	p, ctx, sm, _, err := initConstruct(ctrl)
	require.NoError(err)

	psc, ok := p.(protocol.PreStatesCreator)
	require.True(ok)
	bcCtx := protocol.MustGetBlockchainCtx(ctx)
	rp := rolldpos.MustGetProtocol(bcCtx.Registry)

	test := make(map[uint64](map[string]uint32))
	test[2] = map[string]uint32{
		identityset.Address(1).String(): 1, // [A, B, C]
		identityset.Address(2).String(): 1,
		identityset.Address(3).String(): 1,
	}
	test[3] = map[string]uint32{
		identityset.Address(1).String(): 1, // [A, B, C, D]
		identityset.Address(2).String(): 2,
		identityset.Address(3).String(): 1,
		identityset.Address(4).String(): 1,
	}
	test[4] = map[string]uint32{
		identityset.Address(2).String(): 1, // [B, D, E, F]
		identityset.Address(4).String(): 1,
		identityset.Address(5).String(): 1,
		identityset.Address(6).String(): 1,
	}

	// testing for kick-out slashing
	var epochNum uint64
	for epochNum = 1; epochNum <= 3; epochNum++ {
		if epochNum > 1 {
			epochStartHeight := rp.GetEpochHeight(epochNum)
			ctx = protocol.WithBlockCtx(
				ctx,
				protocol.BlockCtx{
					BlockHeight: epochStartHeight,
					Producer:    identityset.Address(1),
				},
			)
			require.NoError(psc.CreatePreStates(ctx, sm)) // shift
			bl := &vote.Blacklist{}
			candKey := candidatesutil.ConstructKey(candidatesutil.CurKickoutKey)
			_, err := sm.State(bl, protocol.KeyOption(candKey[:]), protocol.NamespaceOption(protocol.SystemNamespace))
			require.NoError(err)
			expected := test[epochNum]
			require.Equal(len(expected), len(bl.BlacklistInfos))
			for addr, count := range bl.BlacklistInfos {
				val, ok := expected[addr]
				require.True(ok)
				require.Equal(val, count)
			}
		}
		// at last of epoch, set blacklist into next kickout key
		epochLastHeight := rp.GetEpochLastBlockHeight(epochNum)
		ctx = protocol.WithBlockCtx(
			ctx,
			protocol.BlockCtx{
				BlockHeight: epochLastHeight,
				Producer:    identityset.Address(1),
			},
		)
		require.NoError(psc.CreatePreStates(ctx, sm))

		bl := &vote.Blacklist{}
		candKey := candidatesutil.ConstructKey(candidatesutil.NxtKickoutKey)
		_, err = sm.State(bl, protocol.KeyOption(candKey[:]), protocol.NamespaceOption(protocol.SystemNamespace))
		require.NoError(err)
		expected := test[epochNum+1]
		require.Equal(len(expected), len(bl.BlacklistInfos))
		for addr, count := range bl.BlacklistInfos {
			val, ok := expected[addr]
			require.True(ok)
			require.Equal(val, count)
		}
	}
}

func TestHandle(t *testing.T) {
	require := require.New(t)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	p, ctx, sm, _, err := initConstruct(ctrl)
	require.NoError(err)
	require.NoError(p.CreateGenesisStates(ctx, sm))

	// wrong action
	recipientAddr := identityset.Address(28)
	senderKey := identityset.PrivateKey(27)
	tsf, err := action.NewTransfer(0, big.NewInt(10), recipientAddr.String(), []byte{}, uint64(100000), big.NewInt(10))
	require.NoError(err)
	bd := &action.EnvelopeBuilder{}
	elp := bd.SetGasLimit(uint64(100000)).
		SetGasPrice(big.NewInt(10)).
		SetAction(tsf).Build()
	selp, err := action.Sign(elp, senderKey)
	require.NoError(err)
	require.NotNil(selp)
	// Case 1: wrong action type
	receipt, err := p.Handle(ctx, selp.Action(), nil)
	require.NoError(err)
	require.Nil(receipt)
	// Case 2: all right
	p2, ctx2, sm2, _, err := initConstruct(ctrl)
	require.NoError(err)
	require.NoError(p2.CreateGenesisStates(ctx2, sm2))
	var sc2 state.CandidateList
	candKey := candidatesutil.ConstructKey(candidatesutil.NxtCandidateKey)
	_, err = sm2.State(&sc2, protocol.KeyOption(candKey[:]), protocol.NamespaceOption(protocol.SystemNamespace))
	require.NoError(err)
	act2 := action.NewPutPollResult(1, 1, sc2)
	elp = bd.SetGasLimit(uint64(100000)).
		SetGasPrice(big.NewInt(10)).
		SetAction(act2).Build()
	selp2, err := action.Sign(elp, senderKey)
	require.NoError(err)
	require.NotNil(selp2)
	receipt, err = p.Handle(ctx2, selp2.Action(), sm2)
	require.NoError(err)
	require.NotNil(receipt)

	_, err = shiftCandidates(sm2)
	require.NoError(err)
	candidates, _, err := candidatesutil.CandidatesFromDB(sm2, false)
	require.NoError(err)
	require.Equal(2, len(candidates))
	require.Equal(candidates[0].Address, sc2[0].Address)
	require.Equal(candidates[0].Votes, sc2[0].Votes)
	require.Equal(candidates[1].Address, sc2[1].Address)
	require.Equal(candidates[1].Votes, sc2[1].Votes)
}

func TestProtocol_Validate(t *testing.T) {
	require := require.New(t)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	p, ctx, sm, _, err := initConstruct(ctrl)
	require.NoError(err)
	require.NoError(p.CreateGenesisStates(ctx, sm))

	// wrong action
	recipientAddr := identityset.Address(28)
	senderKey := identityset.PrivateKey(27)
	tsf, err := action.NewTransfer(0, big.NewInt(10), recipientAddr.String(), []byte{}, uint64(100000), big.NewInt(10))
	require.NoError(err)
	bd := &action.EnvelopeBuilder{}
	elp := bd.SetGasLimit(uint64(100000)).
		SetGasPrice(big.NewInt(10)).
		SetAction(tsf).Build()
	selp, err := action.Sign(elp, senderKey)
	require.NoError(err)
	require.NotNil(selp)
	// Case 1: wrong action type
	require.NoError(p.Validate(ctx, selp.Action()))
	// Case 2: Only producer could create this protocol
	p2, ctx2, sm2, _, err := initConstruct(ctrl)
	require.NoError(err)
	require.NoError(p2.CreateGenesisStates(ctx2, sm2))
	var sc2 state.CandidateList
	candKey := candidatesutil.ConstructKey(candidatesutil.NxtCandidateKey)
	_, err = sm2.State(&sc2, protocol.KeyOption(candKey[:]), protocol.NamespaceOption(protocol.SystemNamespace))
	require.NoError(err)
	act2 := action.NewPutPollResult(1, 1, sc2)
	elp = bd.SetGasLimit(uint64(100000)).
		SetGasPrice(big.NewInt(10)).
		SetAction(act2).Build()
	selp2, err := action.Sign(elp, senderKey)
	require.NoError(err)
	require.NotNil(selp2)
	caller, err := address.FromBytes(selp.SrcPubkey().Hash())
	require.NoError(err)
	ctx2 = protocol.WithBlockCtx(
		ctx2,
		protocol.BlockCtx{
			BlockHeight: 1,
			Producer:    recipientAddr,
		},
	)
	ctx2 = protocol.WithActionCtx(
		ctx2,
		protocol.ActionCtx{
			Caller: caller,
		},
	)
	err = p.Validate(ctx2, selp2.Action())
	require.True(strings.Contains(err.Error(), "Only producer could create this protocol"))
	// Case 3: duplicate candidate
	p3, ctx3, sm3, _, err := initConstruct(ctrl)
	require.NoError(err)
	require.NoError(p3.CreateGenesisStates(ctx3, sm3))
	var sc3 state.CandidateList
	_, err = sm3.State(&sc3, protocol.KeyOption(candKey[:]), protocol.NamespaceOption(protocol.SystemNamespace))
	require.NoError(err)
	sc3 = append(sc3, &state.Candidate{"1", big.NewInt(10), "2", nil})
	sc3 = append(sc3, &state.Candidate{"1", big.NewInt(10), "2", nil})
	act3 := action.NewPutPollResult(1, 1, sc3)
	elp = bd.SetGasLimit(uint64(100000)).
		SetGasPrice(big.NewInt(10)).
		SetAction(act3).Build()
	selp3, err := action.Sign(elp, senderKey)
	require.NoError(err)
	require.NotNil(selp3)
	ctx3 = protocol.WithBlockCtx(
		ctx3,
		protocol.BlockCtx{
			BlockHeight: 1,
			Producer:    identityset.Address(27),
		},
	)
	ctx3 = protocol.WithActionCtx(
		ctx3,
		protocol.ActionCtx{
			Caller: caller,
		},
	)
	err = p.Validate(ctx3, selp3.Action())
	require.True(strings.Contains(err.Error(), "duplicate candidate"))

	// Case 4: delegate's length is not equal
	p4, ctx4, sm4, _, err := initConstruct(ctrl)
	require.NoError(err)
	require.NoError(p4.CreateGenesisStates(ctx4, sm4))
	var sc4 state.CandidateList
	_, err = sm4.State(&sc4, protocol.KeyOption(candKey[:]), protocol.NamespaceOption(protocol.SystemNamespace))
	require.NoError(err)
	sc4 = append(sc4, &state.Candidate{"1", big.NewInt(10), "2", nil})
	act4 := action.NewPutPollResult(1, 1, sc4)
	bd4 := &action.EnvelopeBuilder{}
	elp4 := bd4.SetGasLimit(uint64(100000)).
		SetGasPrice(big.NewInt(10)).
		SetAction(act4).Build()
	selp4, err := action.Sign(elp4, senderKey)
	require.NoError(err)
	require.NotNil(selp4)
	ctx4 = protocol.WithBlockCtx(
		ctx4,
		protocol.BlockCtx{
			BlockHeight: 1,
			Producer:    identityset.Address(27),
		},
	)
	ctx4 = protocol.WithActionCtx(
		ctx4,
		protocol.ActionCtx{
			Caller: caller,
		},
	)
	err = p4.Validate(ctx4, selp4.Action())
	require.True(strings.Contains(err.Error(), "the proposed delegate list length"))
	// Case 5: candidate's vote is not equal
	p5, ctx5, sm5, _, err := initConstruct(ctrl)
	require.NoError(err)
	require.NoError(p5.CreateGenesisStates(ctx5, sm5))
	var sc5 state.CandidateList
	_, err = sm5.State(&sc5, protocol.KeyOption(candKey[:]), protocol.NamespaceOption(protocol.SystemNamespace))
	require.NoError(err)
	sc5[0].Votes = big.NewInt(10)
	act5 := action.NewPutPollResult(1, 1, sc5)
	bd5 := &action.EnvelopeBuilder{}
	elp5 := bd5.SetGasLimit(uint64(100000)).
		SetGasPrice(big.NewInt(10)).
		SetAction(act5).Build()
	selp5, err := action.Sign(elp5, senderKey)
	require.NoError(err)
	require.NotNil(selp5)
	ctx5 = protocol.WithBlockCtx(
		ctx5,
		protocol.BlockCtx{
			BlockHeight: 1,
			Producer:    identityset.Address(27),
		},
	)
	ctx5 = protocol.WithActionCtx(
		ctx5,
		protocol.ActionCtx{
			Caller: caller,
		},
	)
	err = p5.Validate(ctx5, selp5.Action())
	require.True(strings.Contains(err.Error(), "delegates are not as expected"))
	// Case 6: all good
	p6, ctx6, sm6, _, err := initConstruct(ctrl)
	require.NoError(err)
	require.NoError(p6.CreateGenesisStates(ctx6, sm6))
	var sc6 state.CandidateList
	_, err = sm6.State(&sc6, protocol.KeyOption(candKey[:]), protocol.NamespaceOption(protocol.SystemNamespace))
	require.NoError(err)
	act6 := action.NewPutPollResult(1, 1, sc6)
	bd6 := &action.EnvelopeBuilder{}
	elp6 := bd6.SetGasLimit(uint64(100000)).
		SetGasPrice(big.NewInt(10)).
		SetAction(act6).Build()
	selp6, err := action.Sign(elp6, senderKey)
	require.NoError(err)
	require.NotNil(selp6)
	caller6, err := address.FromBytes(selp6.SrcPubkey().Hash())
	require.NoError(err)
	ctx6 = protocol.WithBlockCtx(
		ctx6,
		protocol.BlockCtx{
			BlockHeight: 1,
			Producer:    identityset.Address(27),
		},
	)
	ctx6 = protocol.WithActionCtx(
		ctx6,
		protocol.ActionCtx{
			Caller: caller6,
		},
	)
	require.NoError(p6.Validate(ctx6, selp6.Action()))
}

func TestCandidatesByHeight(t *testing.T) {
	require := require.New(t)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	p, ctx, sm, _, err := initConstruct(ctrl)
	require.NoError(err)
	blackListMap := map[string]uint32{
		identityset.Address(1).String(): 1,
		identityset.Address(2).String(): 1,
	}

	blackList := &vote.Blacklist{
		BlacklistInfos: blackListMap,
		IntensityRate:  50,
	}
	require.NoError(setNextEpochBlacklist(sm, nil, 721, blackList))
	filteredCandidates, err := p.CandidatesByHeight(ctx, 721)
	require.NoError(err)
	require.Equal(4, len(filteredCandidates))

	for _, cand := range filteredCandidates {
		if cand.Address == identityset.Address(1).String() {
			require.Equal(0, cand.Votes.Cmp(big.NewInt(15)))
		}
		if cand.Address == identityset.Address(2).String() {
			require.Equal(0, cand.Votes.Cmp(big.NewInt(11)))
		}
	}

	// change intensity rate to be 0
	blackList = &vote.Blacklist{
		BlacklistInfos: blackListMap,
		IntensityRate:  0,
	}
	require.NoError(setNextEpochBlacklist(sm, nil, 721, blackList))
	filteredCandidates, err = p.CandidatesByHeight(ctx, 721)
	require.NoError(err)
	require.Equal(4, len(filteredCandidates))

	for _, cand := range filteredCandidates {
		if cand.Address == identityset.Address(1).String() {
			require.Equal(0, cand.Votes.Cmp(big.NewInt(30)))
		}
		if cand.Address == identityset.Address(2).String() {
			require.Equal(0, cand.Votes.Cmp(big.NewInt(22)))
		}
	}

}

func TestDelegatesByEpoch(t *testing.T) {
	require := require.New(t)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	p, ctx, sm, _, err := initConstruct(ctrl)
	require.NoError(err)

	// 1: empty blacklist DelegatesByEpoch()
	blackListMap := map[string]uint32{}
	blackList := &vote.Blacklist{
		BlacklistInfos: blackListMap,
		IntensityRate:  90,
	}
	require.NoError(setNextEpochBlacklist(sm, nil, 721, blackList))

	delegates, err := p.DelegatesByEpoch(ctx, 2)
	require.NoError(err)
	require.Equal(2, len(delegates))
	require.Equal(identityset.Address(2).String(), delegates[0].Address)
	require.Equal(identityset.Address(1).String(), delegates[1].Address)

	// 2: not empty blacklist DelegatesByEpoch()
	blackListMap2 := map[string]uint32{
		identityset.Address(1).String(): 1,
		identityset.Address(2).String(): 1,
	}
	blackList2 := &vote.Blacklist{
		BlacklistInfos: blackListMap2,
		IntensityRate:  90,
	}
	require.NoError(setNextEpochBlacklist(sm, nil, 721, blackList2))
	delegates2, err := p.DelegatesByEpoch(ctx, 2)
	require.NoError(err)
	require.Equal(2, len(delegates2))
	// even though the address 1, 2 have larger amount of votes, it got kicked out because it's on kick-out list
	require.Equal(identityset.Address(3).String(), delegates2[0].Address)
	require.Equal(identityset.Address(4).String(), delegates2[1].Address)

	// 3: kickout out with different blacklist
	blackListMap3 := map[string]uint32{
		identityset.Address(1).String(): 1,
		identityset.Address(3).String(): 2,
	}
	blackList3 := &vote.Blacklist{
		BlacklistInfos: blackListMap3,
		IntensityRate:  90,
	}
	require.NoError(setNextEpochBlacklist(sm, nil, 721, blackList3))

	delegates3, err := p.DelegatesByEpoch(ctx, 2)
	require.NoError(err)

	require.Equal(2, len(delegates3))
	require.Equal(identityset.Address(2).String(), delegates3[0].Address)
	require.Equal(identityset.Address(4).String(), delegates3[1].Address)

	// 4: shift kickout list and Delegates()
	_, err = shiftKickoutList(sm)
	require.NoError(err)
	delegates4, err := p.DelegatesByEpoch(ctx, 1)
	require.NoError(err)
	require.Equal(len(delegates4), len(delegates3))
	for i, d := range delegates3 {
		require.True(d.Equal(delegates4[i]))
	}

	// 5: test hard kick-out
	blackListMap5 := map[string]uint32{
		identityset.Address(1).String(): 1,
		identityset.Address(2).String(): 2,
		identityset.Address(3).String(): 2,
	}
	blackList5 := &vote.Blacklist{
		BlacklistInfos: blackListMap5,
		IntensityRate:  100, // hard kickout
	}
	require.NoError(setNextEpochBlacklist(sm, nil, 721, blackList5))

	delegates5, err := p.DelegatesByEpoch(ctx, 2)
	require.NoError(err)

	require.Equal(1, len(delegates5)) // exclude all of them
	require.Equal(identityset.Address(4).String(), delegates5[0].Address)
}
