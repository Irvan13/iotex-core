// Copyright (c) 2020 IoTeX Foundation
// This is an alpha (internal) release and is not suitable for production. This source code is provided 'as is' and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package staking

import (
	"context"
	"math/big"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/iotexproject/go-pkgs/hash"
	"github.com/iotexproject/iotex-address/address"
	"github.com/iotexproject/iotex-proto/golang/iotexapi"

	"github.com/iotexproject/iotex-core/action"
	"github.com/iotexproject/iotex-core/action/protocol"
	"github.com/iotexproject/iotex-core/blockchain/genesis"
	"github.com/iotexproject/iotex-core/pkg/log"
	"github.com/iotexproject/iotex-core/pkg/unit"
	"github.com/iotexproject/iotex-core/state"
	"github.com/iotexproject/iotex-core/state/factory"
)

// protocolID is the protocol ID
const protocolID = "staking"

// Errors
var (
	ErrAlreadyExist = errors.New("candidate already exist")
)

// Protocol defines the protocol of handling staking
type Protocol struct {
	addr            address.Address
	inMemCandidates *CandidateCenter
	depositGas      DepositGas
	sr              protocol.StateReader
	config          genesis.Staking
}

// DepositGas deposits gas to some pool
type DepositGas func(ctx context.Context, sm protocol.StateManager, amount *big.Int) error

// NewProtocol instantiates the protocol of staking
func NewProtocol(depositGas DepositGas, sr protocol.StateReader, cfg genesis.Staking) *Protocol {
	h := hash.Hash160b([]byte(protocolID))
	addr, err := address.FromBytes(h[:])
	if err != nil {
		log.L().Panic("Error when constructing the address of staking protocol", zap.Error(err))
	}

	return &Protocol{
		addr:            addr,
		inMemCandidates: NewCandidateCenter(),
		config:          cfg,
		depositGas:      depositGas,
		sr:              sr,
	}
}

// CreateGenesisStates is used to setup BootstrapCandidates from genesis config.
func (p *Protocol) CreateGenesisStates(
	ctx context.Context,
	sm protocol.StateManager,
) error {
	for _, bc := range p.config.BootstrapCandidates {
		owner, err := address.FromString(bc.OwnerAddress)
		if err != nil {
			return err
		}

		operator, err := address.FromString(bc.OperatorAddress)
		if err != nil {
			return err
		}

		reward, err := address.FromString(bc.RewardAddress)
		if err != nil {
			return err
		}

		selfStake := unit.ConvertIotxToRau(bc.SelfStakingTokens)
		bucket := NewVoteBucket(owner, owner, selfStake, 7, time.Now(), true)
		bucketIdx, err := putBucketAndIndex(sm, bucket)
		if err != nil {
			return err
		}
		c := &Candidate{
			Owner:              owner,
			Operator:           operator,
			Reward:             reward,
			Name:               bc.Name,
			Votes:              p.calculateVoteWeight(bucket, true),
			SelfStakeBucketIdx: bucketIdx,
			SelfStake:          selfStake,
		}

		// put in statedb
		if err := putCandidate(sm, c); err != nil {
			return err
		}
		// put in mem
		if err := p.inMemCandidates.Upsert(c); err != nil {
			return err
		}
	}
	return nil
}

// Start starts the protocol
func (p *Protocol) Start(ctx context.Context) error {
	// read all candidates from stateDB
	_, iter, err := p.sr.States(protocol.NamespaceOption(factory.CandidateNameSpace))
	if errors.Cause(err) == state.ErrStateNotExist {
		return nil
	}
	if err != nil {
		return err
	}

	// decode the candidate and put into candidate center
	for i := 0; i < iter.Size(); i++ {
		c := &Candidate{}
		if err := iter.Next(c); err != nil {
			return errors.Wrapf(err, "failed to deserialize candidate")
		}

		if err := p.inMemCandidates.Upsert(c); err != nil {
			return err
		}
	}
	return nil
}

// Handle handles a staking message
func (p *Protocol) Handle(ctx context.Context, act action.Action, sm protocol.StateManager) (*action.Receipt, error) {
	switch act := act.(type) {
	case *action.CreateStake:
		return p.handleCreateStake(ctx, act, sm)
	case *action.Unstake:
		return p.handleUnstake(ctx, act, sm)
	case *action.WithdrawStake:
		return p.handleWithdrawStake(ctx, act, sm)
	case *action.ChangeCandidate:
		return p.handleChangeCandidate(ctx, act, sm)
	case *action.TransferStake:
		return p.handleTransferStake(ctx, act, sm)
	case *action.DepositToStake:
		return p.handleDepositToStake(ctx, act, sm)
	case *action.Restake:
		return p.handleRestake(ctx, act, sm)
	case *action.CandidateRegister:
		return p.handleCandidateRegister(ctx, act, sm)
	case *action.CandidateUpdate:
		return p.handleCandidateUpdate(ctx, act, sm)
	}
	return nil, nil
}

// Validate validates a staking message
func (p *Protocol) Validate(ctx context.Context, act action.Action) error {
	switch act := act.(type) {
	case *action.CreateStake:
		return p.validateCreateStake(ctx, act)
	case *action.Unstake:
		return p.validateUnstake(ctx, act)
	case *action.WithdrawStake:
		return p.validateWithdrawStake(ctx, act)
	case *action.ChangeCandidate:
		return p.validateChangeCandidate(ctx, act)
	case *action.TransferStake:
		return p.validateTransferStake(ctx, act)
	case *action.DepositToStake:
		return p.validateDepositToStake(ctx, act)
	case *action.Restake:
		return p.validateRestake(ctx, act)
	case *action.CandidateRegister:
		return p.validateCandidateRegister(ctx, act)
	case *action.CandidateUpdate:
		return p.validateCandidateUpdate(ctx, act)
	}
	return nil
}

// ReadState read the state on blockchain via protocol
func (p *Protocol) ReadState(ctx context.Context, sr protocol.StateReader, method []byte, args ...[]byte) ([]byte, error) {
	m := iotexapi.ReadStakingDataMethod{}
	if err := proto.Unmarshal(method, &m); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal method name")
	}
	if len(args) != 1 {
		return nil, errors.Errorf("invalid number of arguments %d", len(args))
	}
	r := iotexapi.ReadStakingDataRequest{}
	if err := proto.Unmarshal(args[0], &r); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal request")
	}
	var (
		resp proto.Message
		err  error
	)
	switch m.GetMethod() {
	case iotexapi.ReadStakingDataMethod_BUCKETS:
		resp, err = p.readStateBuckets(ctx, sr, r.GetBuckets())
	case iotexapi.ReadStakingDataMethod_BUCKETS_BY_VOTER:
		resp, err = p.readStateBucketsByVoter(ctx, sr, r.GetBucketsByVoter())
	case iotexapi.ReadStakingDataMethod_BUCKETS_BY_CANDIDATE:
		resp, err = p.readStateBucketsByCandidate(ctx, sr, r.GetBucketsByCandidate())
	case iotexapi.ReadStakingDataMethod_CANDIDATES:
	case iotexapi.ReadStakingDataMethod_CANDIDATE_BY_NAME:
	default:
		err = errors.New("corresponding method isn't found")
	}
	if err != nil {
		return nil, err
	}

	return proto.Marshal(resp)
}

// Register registers the protocol with a unique ID
func (p *Protocol) Register(r *protocol.Registry) error {
	return r.Register(protocolID, p)
}

// ForceRegister registers the protocol with a unique ID and force replacing the previous protocol if it exists
func (p *Protocol) ForceRegister(r *protocol.Registry) error {
	return r.ForceRegister(protocolID, p)
}

func (p *Protocol) calculateVoteWeight(v *VoteBucket, selfStake bool) *big.Int {
	return calculateVoteWeight(p.config.VoteWeightCalConsts, v, selfStake)
}
