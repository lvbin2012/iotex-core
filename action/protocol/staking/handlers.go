// Copyright (c) 2020 IoTeX Foundation
// This is an alpha (internal) release and is not suitable for production. This source code is provided 'as is' and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package staking

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/iotexproject/go-pkgs/byteutil"
	"github.com/iotexproject/go-pkgs/hash"
	"github.com/iotexproject/iotex-address/address"
	"github.com/iotexproject/iotex-proto/golang/iotextypes"

	"github.com/iotexproject/iotex-core/action"
	"github.com/iotexproject/iotex-core/action/protocol"
	accountutil "github.com/iotexproject/iotex-core/action/protocol/account/util"
	"github.com/iotexproject/iotex-core/pkg/log"
	"github.com/iotexproject/iotex-core/state"
)

const (
	// HandleCreateStake is the handler name of createStake
	HandleCreateStake = "createStake"
	// HandleUnstake is the handler name of unstake
	HandleUnstake = "unstake"
	// HandleWithdrawStake is the handler name of withdrawStake
	HandleWithdrawStake = "withdrawStake"
	// HandleChangeCandidate is the handler name of changeCandidate
	HandleChangeCandidate = "changeCandidate"
	// HandleTransferStake is the handler name of transferStake
	HandleTransferStake = "transferStake"
	// HandleDepositToStake is the handler name of depositToStake
	HandleDepositToStake = "depositToStake"
	// HandleRestake is the handler name of restake
	HandleRestake = "restake"
	// HandleCandidateRegister is the handler name of candidateRegister
	HandleCandidateRegister = "candidateRegister"
	// HandleCandidateUpdate is the handler name of candidateUpdate
	HandleCandidateUpdate = "candidateUpdate"
)

type fetchError struct {
	err           error
	failureStatus iotextypes.ReceiptStatus
}

func (p *Protocol) handleCreateStake(ctx context.Context, act *action.CreateStake, sm protocol.StateManager) (*action.Receipt, error) {
	actionCtx := protocol.MustGetActionCtx(ctx)
	blkCtx := protocol.MustGetBlockCtx(ctx)

	staker, gasFee, fetchErr := fetchCaller(ctx, sm, act.Amount())
	if fetchErr != nil {
		if fetchErr.failureStatus == iotextypes.ReceiptStatus_Failure {
			return nil, fetchErr.err
		}
		log.L().Debug("Error when fetching caller", zap.Error(fetchErr.err))
		return p.settleAction(ctx, sm, uint64(fetchErr.failureStatus), gasFee)
	}

	// Create new bucket and bucket index
	candidate := p.inMemCandidates.GetByName(act.Candidate())
	if candidate == nil {
		log.L().Debug("Error when finding candidate in candidate center", zap.Error(ErrInvalidCanName))
		return p.settleAction(ctx, sm, uint64(iotextypes.ReceiptStatus_ErrCandidateNotExist), gasFee)
	}
	bucket := NewVoteBucket(candidate.Owner, actionCtx.Caller, act.Amount(), act.Duration(), blkCtx.BlockTimeStamp, act.AutoStake())
	bucketIdx, err := putBucketAndIndex(sm, bucket)
	if err != nil {
		return nil, errors.Wrap(err, "failed to put bucket")
	}

	// update candidate
	weightedVote := p.calculateVoteWeight(bucket, false)
	if err := candidate.AddVote(weightedVote); err != nil {
		return nil, errors.Wrapf(err, "failed to add vote for candidate %s", candidate.Owner.String())
	}
	if err := putCandidate(sm, candidate); err != nil {
		return nil, errors.Wrapf(err, "failed to put state of candidate %s", candidate.Owner.String())
	}

	// update staker balance
	if err := staker.SubBalance(act.Amount()); err != nil {
		return nil, errors.Wrapf(err, "failed to update the balance of staker %s", actionCtx.Caller.String())
	}
	// put updated staker's account state to trie
	if err := accountutil.StoreAccount(sm, actionCtx.Caller.String(), staker); err != nil {
		return nil, errors.Wrapf(err, "failed to store account %s", actionCtx.Caller.String())
	}

	log := p.createLog(ctx, HandleCreateStake, candidate.Owner, actionCtx.Caller, byteutil.Uint64ToBytes(bucketIdx))
	receipt, err := p.settleAction(ctx, sm, uint64(iotextypes.ReceiptStatus_Success), gasFee, log)
	if err != nil {
		return nil, errors.Wrap(err, "failed to settle action")
	}
	if err := p.inMemCandidates.Upsert(candidate); err != nil {
		return nil, err
	}
	return receipt, nil
}

func (p *Protocol) handleUnstake(ctx context.Context, act *action.Unstake, sm protocol.StateManager) (*action.Receipt, error) {
	actionCtx := protocol.MustGetActionCtx(ctx)
	blkCtx := protocol.MustGetBlockCtx(ctx)

	_, gasFee, fetchErr := fetchCaller(ctx, sm, big.NewInt(0))
	if fetchErr != nil {
		if fetchErr.failureStatus == iotextypes.ReceiptStatus_Failure {
			return nil, fetchErr.err
		}
		log.L().Debug("Error when fetching caller", zap.Error(fetchErr.err))
		return p.settleAction(ctx, sm, uint64(fetchErr.failureStatus), gasFee)
	}

	bucket, fetchErr := p.fetchBucket(ctx, sm, act.BucketIndex(), true, true)
	if fetchErr != nil {
		if fetchErr.failureStatus == iotextypes.ReceiptStatus_Failure {
			return nil, fetchErr.err
		}
		log.L().Debug("Error when fetching bucket", zap.Error(fetchErr.err))
		return p.settleAction(ctx, sm, uint64(fetchErr.failureStatus), gasFee)
	}

	// update bucket
	bucket.UnstakeStartTime = blkCtx.BlockTimeStamp
	if err := updateBucket(sm, act.BucketIndex(), bucket); err != nil {
		return nil, errors.Wrapf(err, "failed to update bucket for voter %s", bucket.Owner)
	}

	candidate := p.inMemCandidates.GetByOwner(bucket.Candidate)
	if candidate == nil {
		return nil, errors.Wrap(ErrInvalidOwner, "cannot find candidate in candidate center")
	}
	weightedVote := p.calculateVoteWeight(bucket, p.inMemCandidates.ContainsSelfStakingBucket(act.BucketIndex()))
	if err := candidate.SubVote(weightedVote); err != nil {
		return nil, errors.Wrapf(err, "failed to subtract vote for candidate %s", bucket.Candidate.String())
	}
	// clear candidate's self stake if the bucket is self staking
	if p.inMemCandidates.ContainsSelfStakingBucket(act.BucketIndex()) {
		candidate.SelfStake = big.NewInt(0)
	}
	if err := putCandidate(sm, candidate); err != nil {
		return nil, errors.Wrapf(err, "failed to put state of candidate %s", bucket.Candidate.String())
	}

	log := p.createLog(ctx, HandleUnstake, nil, actionCtx.Caller, nil)
	receipt, err := p.settleAction(ctx, sm, uint64(iotextypes.ReceiptStatus_Success), gasFee, log)
	if err != nil {
		return nil, errors.Wrap(err, "failed to settle action")
	}
	if err := p.inMemCandidates.Upsert(candidate); err != nil {
		return nil, err
	}
	return receipt, nil
}

func (p *Protocol) handleWithdrawStake(ctx context.Context, act *action.WithdrawStake, sm protocol.StateManager) (*action.Receipt, error) {
	actionCtx := protocol.MustGetActionCtx(ctx)
	blkCtx := protocol.MustGetBlockCtx(ctx)

	withdrawer, gasFee, fetchErr := fetchCaller(ctx, sm, big.NewInt(0))
	if fetchErr != nil {
		if fetchErr.failureStatus == iotextypes.ReceiptStatus_Failure {
			return nil, fetchErr.err
		}
		log.L().Debug("Error when fetching caller", zap.Error(fetchErr.err))
		return p.settleAction(ctx, sm, uint64(fetchErr.failureStatus), gasFee)
	}

	bucket, fetchErr := p.fetchBucket(ctx, sm, act.BucketIndex(), true, true)
	if fetchErr != nil {
		if fetchErr.failureStatus == iotextypes.ReceiptStatus_Failure {
			return nil, fetchErr.err
		}
		log.L().Debug("Error when fetching bucket", zap.Error(fetchErr.err))
		return p.settleAction(ctx, sm, uint64(fetchErr.failureStatus), gasFee)
	}

	// check unstake time
	if bucket.UnstakeStartTime.Unix() == 0 {
		err := errors.New("bucket has not been unstaked")
		log.L().Debug("Error when withdrawing bucket", zap.Error(err))
		return p.settleAction(ctx, sm, uint64(iotextypes.ReceiptStatus_ErrWithdrawBeforeUnstake), gasFee)
	}
	if blkCtx.BlockTimeStamp.Before(bucket.UnstakeStartTime.Add(p.config.WithdrawWaitingPeriod)) {
		err := fmt.Errorf("stake is not ready to withdraw, current time %s, required time %s",
			blkCtx.BlockTimeStamp, bucket.UnstakeStartTime.Add(p.config.WithdrawWaitingPeriod))
		log.L().Debug("Error when withdrawing bucket", zap.Error(err))
		return p.settleAction(ctx, sm, uint64(iotextypes.ReceiptStatus_ErrWithdrawBeforeMaturity), gasFee)
	}

	// delete bucket and bucket index
	if err := delBucket(sm, act.BucketIndex()); err != nil {
		return nil, errors.Wrapf(err, "failed to delete bucket for candidate %s", bucket.Candidate.String())
	}
	if err := delCandBucketIndex(sm, bucket.Candidate, act.BucketIndex()); err != nil {
		return nil, errors.Wrapf(err, "failed to delete bucket index for candidate %s", bucket.Candidate.String())
	}
	if err := delVoterBucketIndex(sm, bucket.Owner, act.BucketIndex()); err != nil {
		return nil, errors.Wrapf(err, "failed to delete bucket index for voter %s", bucket.Owner.String())
	}

	// update withdrawer balance
	if err := withdrawer.AddBalance(bucket.StakedAmount); err != nil {
		return nil, errors.Wrapf(err, "failed to update the balance of withdrawer %s", actionCtx.Caller.String())
	}
	// put updated withdrawer's account state to trie
	if err := accountutil.StoreAccount(sm, actionCtx.Caller.String(), withdrawer); err != nil {
		return nil, errors.Wrapf(err, "failed to store account %s", actionCtx.Caller.String())
	}

	log := p.createLog(ctx, HandleWithdrawStake, nil, actionCtx.Caller, nil)
	return p.settleAction(ctx, sm, uint64(iotextypes.ReceiptStatus_Success), gasFee, log)
}

func (p *Protocol) handleChangeCandidate(ctx context.Context, act *action.ChangeCandidate, sm protocol.StateManager) (*action.Receipt, error) {
	actionCtx := protocol.MustGetActionCtx(ctx)

	_, gasFee, fetchErr := fetchCaller(ctx, sm, big.NewInt(0))
	if fetchErr != nil {
		if fetchErr.failureStatus == iotextypes.ReceiptStatus_Failure {
			return nil, fetchErr.err
		}
		log.L().Debug("Error when fetching caller", zap.Error(fetchErr.err))
		return p.settleAction(ctx, sm, uint64(fetchErr.failureStatus), gasFee)
	}

	candidate := p.inMemCandidates.GetByName(act.Candidate())
	if candidate == nil {
		log.L().Debug("Error when finding candidate in candidate center", zap.Error(ErrInvalidCanName))
		return p.settleAction(ctx, sm, uint64(iotextypes.ReceiptStatus_ErrCandidateNotExist), gasFee)
	}

	bucket, fetchErr := p.fetchBucket(ctx, sm, act.BucketIndex(), true, false)
	if fetchErr != nil {
		if fetchErr.failureStatus == iotextypes.ReceiptStatus_Failure {
			return nil, fetchErr.err
		}
		log.L().Debug("Error when fetching bucket", zap.Error(fetchErr.err))
		return p.settleAction(ctx, sm, uint64(fetchErr.failureStatus), gasFee)
	}

	prevCandidate := p.inMemCandidates.GetByOwner(bucket.Candidate)
	if prevCandidate == nil {
		return nil, errors.Wrap(ErrInvalidOwner, "cannot find candidate in candidate center")
	}

	// update bucket index
	if err := delCandBucketIndex(sm, bucket.Candidate, act.BucketIndex()); err != nil {
		return nil, errors.Wrapf(err, "failed to delete candidate bucket index for candidate %s", bucket.Candidate.String())
	}
	if err := putCandBucketIndex(sm, candidate.Owner, act.BucketIndex()); err != nil {
		return nil, errors.Wrapf(err, "failed to put candidate bucket index for candidate %s", candidate.Owner.String())
	}
	// update bucket
	bucket.Candidate = candidate.Owner
	if err := updateBucket(sm, act.BucketIndex(), bucket); err != nil {
		return nil, errors.Wrapf(err, "failed to update bucket for voter %s", bucket.Owner)
	}

	weightedVotes := p.calculateVoteWeight(bucket, false)

	// update previous candidate
	if err := prevCandidate.SubVote(weightedVotes); err != nil {
		return nil, errors.Wrapf(err, "failed to subtract vote for previous candidate %s", prevCandidate.Owner.String())
	}
	if err := putCandidate(sm, prevCandidate); err != nil {
		return nil, errors.Wrapf(err, "failed to put state of previous candidate %s", prevCandidate.Owner.String())
	}

	// update current candidate
	if err := candidate.AddVote(weightedVotes); err != nil {
		return nil, errors.Wrapf(err, "failed to add vote for candidate %s", candidate.Owner.String())
	}
	if err := putCandidate(sm, candidate); err != nil {
		return nil, errors.Wrapf(err, "failed to put state of candidate %s", candidate.Owner.String())
	}

	log := p.createLog(ctx, HandleChangeCandidate, candidate.Owner, actionCtx.Caller, nil)
	receipt, err := p.settleAction(ctx, sm, uint64(iotextypes.ReceiptStatus_Success), gasFee, log)
	if err != nil {
		return nil, errors.Wrap(err, "failed to settle action")
	}
	if err := p.inMemCandidates.Upsert(prevCandidate); err != nil {
		return nil, err
	}
	if err := p.inMemCandidates.Upsert(candidate); err != nil {
		return nil, err
	}
	return receipt, nil
}

func (p *Protocol) handleTransferStake(ctx context.Context, act *action.TransferStake, sm protocol.StateManager) (*action.Receipt, error) {
	actionCtx := protocol.MustGetActionCtx(ctx)

	_, gasFee, fetchErr := fetchCaller(ctx, sm, big.NewInt(0))
	if fetchErr != nil {
		if fetchErr.failureStatus == iotextypes.ReceiptStatus_Failure {
			return nil, fetchErr.err
		}
		log.L().Debug("Error when fetching caller", zap.Error(fetchErr.err))
		return p.settleAction(ctx, sm, uint64(fetchErr.failureStatus), gasFee)
	}

	bucket, fetchErr := p.fetchBucket(ctx, sm, act.BucketIndex(), true, false)
	if fetchErr != nil {
		if fetchErr.failureStatus == iotextypes.ReceiptStatus_Failure {
			return nil, fetchErr.err
		}
		log.L().Debug("Error when fetching bucket", zap.Error(fetchErr.err))
		return p.settleAction(ctx, sm, uint64(fetchErr.failureStatus), gasFee)
	}

	// update bucket index
	if err := delVoterBucketIndex(sm, bucket.Owner, act.BucketIndex()); err != nil {
		return nil, errors.Wrapf(err, "failed to delete voter bucket index for voter %s", bucket.Owner.String())
	}
	if err := putVoterBucketIndex(sm, act.VoterAddress(), act.BucketIndex()); err != nil {
		return nil, errors.Wrapf(err, "failed to put candidate bucket index for voter %s", act.VoterAddress().String())
	}

	// update bucket
	bucket.Owner = act.VoterAddress()
	if err := updateBucket(sm, act.BucketIndex(), bucket); err != nil {
		return nil, errors.Wrapf(err, "failed to update bucket for voter %s", bucket.Owner)
	}

	log := p.createLog(ctx, HandleTransferStake, nil, actionCtx.Caller, nil)
	return p.settleAction(ctx, sm, uint64(iotextypes.ReceiptStatus_Success), gasFee, log)
}

func (p *Protocol) handleDepositToStake(ctx context.Context, act *action.DepositToStake, sm protocol.StateManager) (*action.Receipt, error) {
	actionCtx := protocol.MustGetActionCtx(ctx)

	depositor, gasFee, fetchErr := fetchCaller(ctx, sm, act.Amount())
	if fetchErr != nil {
		if fetchErr.failureStatus == iotextypes.ReceiptStatus_Failure {
			return nil, fetchErr.err
		}
		log.L().Debug("Error when fetching caller", zap.Error(fetchErr.err))
		return p.settleAction(ctx, sm, uint64(fetchErr.failureStatus), gasFee)
	}

	bucket, fetchErr := p.fetchBucket(ctx, sm, act.BucketIndex(), false, true)
	if fetchErr != nil {
		if fetchErr.failureStatus == iotextypes.ReceiptStatus_Failure {
			return nil, fetchErr.err
		}
		log.L().Debug("Error when fetching bucket", zap.Error(fetchErr.err))
		return p.settleAction(ctx, sm, uint64(fetchErr.failureStatus), gasFee)
	}
	if !bucket.AutoStake {
		err := errors.New("deposit is only allowed on auto-stake bucket")
		log.L().Debug("Error when depositing to stake", zap.Error(err))
		return p.settleAction(ctx, sm, uint64(iotextypes.ReceiptStatus_ErrInvalidBucketType), gasFee)
	}
	candidate := p.inMemCandidates.GetByOwner(bucket.Candidate)
	if candidate == nil {
		return nil, errors.Wrap(ErrInvalidOwner, "cannot find candidate in candidate center")
	}

	prevWeightedVotes := p.calculateVoteWeight(bucket, p.inMemCandidates.ContainsSelfStakingBucket(act.BucketIndex()))
	// update bucket
	bucket.StakedAmount.Add(bucket.StakedAmount, act.Amount())
	if err := updateBucket(sm, act.BucketIndex(), bucket); err != nil {
		return nil, errors.Wrapf(err, "failed to update bucket for voter %s", bucket.Owner)
	}

	// update candidate
	if err := candidate.SubVote(prevWeightedVotes); err != nil {
		return nil, errors.Wrapf(err, "failed to subtract vote for candidate %s", bucket.Candidate.String())
	}
	weightedVotes := p.calculateVoteWeight(bucket, p.inMemCandidates.ContainsSelfStakingBucket(act.BucketIndex()))
	if err := candidate.AddVote(weightedVotes); err != nil {
		return nil, errors.Wrapf(err, "failed to add vote for candidate %s", bucket.Candidate.String())
	}
	if p.inMemCandidates.ContainsSelfStakingBucket(act.BucketIndex()) {
		if err := candidate.AddSelfStake(act.Amount()); err != nil {
			return nil, errors.Wrapf(err, "failed to add self stake for candidate %s", bucket.Candidate.String())
		}
	}
	if err := putCandidate(sm, candidate); err != nil {
		return nil, errors.Wrapf(err, "failed to put state of candidate %s", bucket.Candidate.String())
	}

	// update depositor balance
	if err := depositor.SubBalance(act.Amount()); err != nil {
		return nil, errors.Wrapf(err, "failed to update the balance of depositor %s", actionCtx.Caller.String())
	}
	// put updated depositor's account state to trie
	if err := accountutil.StoreAccount(sm, actionCtx.Caller.String(), depositor); err != nil {
		return nil, errors.Wrapf(err, "failed to store account %s", actionCtx.Caller.String())
	}

	log := p.createLog(ctx, HandleDepositToStake, nil, actionCtx.Caller, nil)
	receipt, err := p.settleAction(ctx, sm, uint64(iotextypes.ReceiptStatus_Success), gasFee, log)
	if err != nil {
		return nil, errors.Wrap(err, "failed to settle action")
	}
	if err := p.inMemCandidates.Upsert(candidate); err != nil {
		return nil, err
	}
	return receipt, nil
}

func (p *Protocol) handleRestake(ctx context.Context, act *action.Restake, sm protocol.StateManager) (*action.Receipt, error) {
	actionCtx := protocol.MustGetActionCtx(ctx)

	_, gasFee, fetchErr := fetchCaller(ctx, sm, big.NewInt(0))
	if fetchErr != nil {
		if fetchErr.failureStatus == iotextypes.ReceiptStatus_Failure {
			return nil, fetchErr.err
		}
		log.L().Debug("Error when fetching caller", zap.Error(fetchErr.err))
		return p.settleAction(ctx, sm, uint64(fetchErr.failureStatus), gasFee)
	}

	bucket, fetchErr := p.fetchBucket(ctx, sm, act.BucketIndex(), true, true)
	if fetchErr != nil {
		if fetchErr.failureStatus == iotextypes.ReceiptStatus_Failure {
			return nil, fetchErr.err
		}
		log.L().Debug("Error when fetching bucket", zap.Error(fetchErr.err))
		return p.settleAction(ctx, sm, uint64(fetchErr.failureStatus), gasFee)
	}

	candidate := p.inMemCandidates.GetByOwner(bucket.Candidate)
	if candidate == nil {
		return nil, errors.Wrap(ErrInvalidOwner, "cannot find candidate in candidate center")
	}

	prevWeightedVotes := p.calculateVoteWeight(bucket, p.inMemCandidates.ContainsSelfStakingBucket(act.BucketIndex()))
	// update bucket
	bucket.StakedDuration = time.Duration(act.Duration()) * 24 * time.Hour
	bucket.AutoStake = act.AutoStake()
	if err := updateBucket(sm, act.BucketIndex(), bucket); err != nil {
		return nil, errors.Wrapf(err, "failed to update bucket for voter %s", bucket.Owner)
	}

	// update candidate
	if err := candidate.SubVote(prevWeightedVotes); err != nil {
		return nil, errors.Wrapf(err, "failed to subtract vote for candidate %s", bucket.Candidate.String())
	}
	weightedVotes := p.calculateVoteWeight(bucket, p.inMemCandidates.ContainsSelfStakingBucket(act.BucketIndex()))
	if err := candidate.AddVote(weightedVotes); err != nil {
		return nil, errors.Wrapf(err, "failed to add vote for candidate %s", bucket.Candidate.String())
	}
	if err := putCandidate(sm, candidate); err != nil {
		return nil, errors.Wrapf(err, "failed to put state of candidate %s", bucket.Candidate.String())
	}

	log := p.createLog(ctx, HandleRestake, nil, actionCtx.Caller, nil)
	receipt, err := p.settleAction(ctx, sm, uint64(iotextypes.ReceiptStatus_Success), gasFee, log)
	if err != nil {
		return nil, errors.Wrap(err, "failed to settle action")
	}
	if err := p.inMemCandidates.Upsert(candidate); err != nil {
		return nil, err
	}
	return receipt, nil
}

func (p *Protocol) handleCandidateRegister(ctx context.Context, act *action.CandidateRegister, sm protocol.StateManager) (*action.Receipt, error) {
	actCtx := protocol.MustGetActionCtx(ctx)
	blkCtx := protocol.MustGetBlockCtx(ctx)

	registrationFee := new(big.Int).Set(p.config.RegistrationConsts.Fee)

	caller, gasFee, fetchErr := fetchCaller(ctx, sm, new(big.Int).Add(act.Amount(), registrationFee))
	if fetchErr != nil {
		if fetchErr.failureStatus == iotextypes.ReceiptStatus_Failure {
			return nil, fetchErr.err
		}
		log.L().Debug("Error when fetching caller", zap.Error(fetchErr.err))
		return p.settleAction(ctx, sm, uint64(fetchErr.failureStatus), gasFee)
	}

	owner := actCtx.Caller
	if act.OwnerAddress() != nil {
		owner = act.OwnerAddress()
	}
	bucket := NewVoteBucket(owner, owner, act.Amount(), act.Duration(), blkCtx.BlockTimeStamp, act.AutoStake())
	bucketIdx, err := putBucketAndIndex(sm, bucket)
	if err != nil {
		return nil, errors.Wrap(err, "failed to put bucket")
	}

	c := &Candidate{
		Owner:              owner,
		Operator:           act.OperatorAddress(),
		Reward:             act.RewardAddress(),
		Name:               act.Name(),
		Votes:              p.calculateVoteWeight(bucket, true),
		SelfStakeBucketIdx: bucketIdx,
		SelfStake:          act.Amount(),
	}

	if err := putCandidate(sm, c); err != nil {
		return nil, err
	}

	// update caller balance
	if err := caller.SubBalance(act.Amount()); err != nil {
		return nil, errors.Wrapf(err, "failed to update the balance of staker %s", actCtx.Caller.String())
	}
	// put updated caller's account state to trie
	if err := accountutil.StoreAccount(sm, actCtx.Caller.String(), caller); err != nil {
		return nil, errors.Wrapf(err, "failed to store account %s", actCtx.Caller.String())
	}

	// put registrationFee to reward pool
	if err := p.depositGas(ctx, sm, registrationFee); err != nil {
		return nil, errors.Wrap(err, "failed to deposit gas")
	}

	log := p.createLog(ctx, HandleCandidateRegister, owner, actCtx.Caller, byteutil.Uint64ToBytes(bucketIdx))
	receipt, err := p.settleAction(ctx, sm, uint64(iotextypes.ReceiptStatus_Success), gasFee, log)
	if err != nil {
		return nil, err
	}

	if err := p.inMemCandidates.Upsert(c); err != nil {
		return nil, err
	}
	return receipt, nil
}

func (p *Protocol) handleCandidateUpdate(ctx context.Context, act *action.CandidateUpdate, sm protocol.StateManager) (*action.Receipt, error) {
	actCtx := protocol.MustGetActionCtx(ctx)

	_, gasFee, fetchErr := fetchCaller(ctx, sm, new(big.Int))
	if fetchErr != nil {
		if fetchErr.failureStatus == iotextypes.ReceiptStatus_Failure {
			return nil, fetchErr.err
		}
		log.L().Debug("Error when fetching caller", zap.Error(fetchErr.err))
		return p.settleAction(ctx, sm, uint64(fetchErr.failureStatus), gasFee)
	}

	// only owner can update candidate
	c := p.inMemCandidates.GetByOwner(actCtx.Caller)
	if c == nil {
		log.L().Debug("Error when updating candidate", zap.Error(ErrInvalidOwner))
		return p.settleAction(ctx, sm, uint64(iotextypes.ReceiptStatus_ErrCandidateNotExist), gasFee)
	}

	if len(act.Name()) != 0 {
		c.Name = act.Name()
	}

	if act.OperatorAddress() != nil {
		c.Operator = act.OperatorAddress()
	}

	if act.RewardAddress() != nil {
		c.Reward = act.RewardAddress()
	}

	if err := putCandidate(sm, c); err != nil {
		return nil, err
	}

	log := p.createLog(ctx, HandleCandidateUpdate, nil, actCtx.Caller, nil)
	receipt, err := p.settleAction(ctx, sm, uint64(iotextypes.ReceiptStatus_Success), gasFee, log)
	if err != nil {
		return nil, err
	}

	if err := p.inMemCandidates.Upsert(c); err != nil {
		return nil, err
	}
	return receipt, nil
}

// settleAccount deposits gas fee and updates caller's nonce
func (p *Protocol) settleAction(
	ctx context.Context,
	sm protocol.StateManager,
	status uint64,
	gasFee *big.Int,
	logs ...*action.Log,
) (*action.Receipt, error) {
	actionCtx := protocol.MustGetActionCtx(ctx)
	blkCtx := protocol.MustGetBlockCtx(ctx)

	if blkCtx.GasLimit < actionCtx.IntrinsicGas {
		return nil, errors.Wrap(action.ErrHitGasLimit, "block gas limit exceeded")
	}
	if err := p.depositGas(ctx, sm, gasFee); err != nil {
		return nil, errors.Wrap(err, "failed to deposit gas")
	}
	if err := p.increaseNonce(sm, actionCtx.Caller, actionCtx.Nonce); err != nil {
		return nil, errors.Wrap(err, "failed to update nonce")
	}
	return &action.Receipt{
		Status:          status,
		BlockHeight:     blkCtx.BlockHeight,
		ActionHash:      actionCtx.ActionHash,
		GasConsumed:     actionCtx.IntrinsicGas,
		ContractAddress: p.addr.String(),
		Logs:            logs,
	}, nil
}

func (p *Protocol) increaseNonce(sm protocol.StateManager, addr address.Address, nonce uint64) error {
	acc, err := accountutil.LoadAccount(sm, hash.BytesToHash160(addr.Bytes()))
	if err != nil {
		return err
	}
	// TODO: this check shouldn't be necessary
	if nonce > acc.Nonce {
		acc.Nonce = nonce
	}
	return accountutil.StoreAccount(sm, addr.String(), acc)
}

func (p *Protocol) fetchBucket(
	ctx context.Context,
	sr protocol.StateReader,
	index uint64,
	checkOwner bool,
	allowSelfStaking bool,
) (*VoteBucket, *fetchError) {
	actionCtx := protocol.MustGetActionCtx(ctx)
	bucket, err := getBucket(sr, index)
	if err != nil {
		fetchErr := &fetchError{
			err:           errors.Wrapf(err, "failed to fetch bucket by index %d", index),
			failureStatus: iotextypes.ReceiptStatus_Failure,
		}
		if errors.Cause(err) == state.ErrStateNotExist {
			fetchErr.failureStatus = iotextypes.ReceiptStatus_ErrInvalidBucketIndex
		}
		return nil, fetchErr
	}
	if checkOwner && !address.Equal(bucket.Owner, actionCtx.Caller) {
		fetchErr := &fetchError{
			err: fmt.Errorf("bucket owner does not match action caller, bucket owner %s, action caller %s",
				bucket.Owner.String(), actionCtx.Caller.String()),
			failureStatus: iotextypes.ReceiptStatus_ErrUnauthorizedOperator,
		}
		return nil, fetchErr
	}
	if !allowSelfStaking && p.inMemCandidates.ContainsSelfStakingBucket(index) {
		fetchErr := &fetchError{
			err:           errors.New("self staking bucket cannot be processed"),
			failureStatus: iotextypes.ReceiptStatus_ErrInvalidBucketType,
		}
		return nil, fetchErr
	}
	return bucket, nil
}

func (p *Protocol) createLog(
	ctx context.Context,
	handlerName string,
	candidateAddr,
	voterAddr address.Address,
	data []byte,
) *action.Log {
	actionCtx := protocol.MustGetActionCtx(ctx)
	blkCtx := protocol.MustGetBlockCtx(ctx)

	topics := []hash.Hash256{hash.Hash256b([]byte(handlerName))}
	if candidateAddr != nil {
		topics = append(topics, hash.Hash256b(candidateAddr.Bytes()))
	}
	topics = append(topics, hash.Hash256b(voterAddr.Bytes()))

	return &action.Log{
		Address:     p.addr.String(),
		Topics:      topics,
		Data:        data,
		BlockHeight: blkCtx.BlockHeight,
		ActionHash:  actionCtx.ActionHash,
	}
}

func putBucketAndIndex(sm protocol.StateManager, bucket *VoteBucket) (uint64, error) {
	index, err := putBucket(sm, bucket)
	if err != nil {
		return 0, errors.Wrap(err, "failed to put bucket")
	}

	if err := putVoterBucketIndex(sm, bucket.Owner, index); err != nil {
		return 0, errors.Wrap(err, "failed to put bucket index")
	}

	if err := putCandBucketIndex(sm, bucket.Candidate, index); err != nil {
		return 0, errors.Wrap(err, "failed to put candidate index")
	}
	return index, nil
}

func fetchCaller(ctx context.Context, sm protocol.StateReader, amount *big.Int) (*state.Account, *big.Int, *fetchError) {
	actionCtx := protocol.MustGetActionCtx(ctx)

	caller, err := accountutil.LoadAccount(sm, hash.BytesToHash160(actionCtx.Caller.Bytes()))
	if err != nil {
		return nil, nil, &fetchError{
			err:           errors.Wrapf(err, "failed to load the account of caller %s", actionCtx.Caller.String()),
			failureStatus: iotextypes.ReceiptStatus_Failure,
		}
	}
	gasFee := big.NewInt(0).Mul(actionCtx.GasPrice, big.NewInt(0).SetUint64(actionCtx.IntrinsicGas))
	// check caller's balance
	if big.NewInt(0).Add(amount, gasFee).Cmp(caller.Balance) == 1 {
		fetchErr := &fetchError{
			err: errors.Wrapf(
				state.ErrNotEnoughBalance,
				"caller %s balance %s, required amount %s",
				actionCtx.Caller.String(),
				caller.Balance,
				big.NewInt(0).Add(amount, gasFee),
			),
			failureStatus: iotextypes.ReceiptStatus_ErrNotEnoughBalance,
		}
		if gasFee.Cmp(caller.Balance) == 1 {
			gasFee = caller.Balance
		}
		return nil, gasFee, fetchErr
	}
	return caller, gasFee, nil
}
