// Copyright IBM Corp. All Rights Reserved.
//
// SPDX-License-Identifier: Apache-2.0
//

package bft

import (
	"math"
	"sync"

	"github.com/SmartBFT-Go/consensus/pkg/types"
	"github.com/SmartBFT-Go/consensus/protos"
)

type Future interface {
	Wait()
}

//go:generate mockery -dir . -name Decider -case underscore -output ./mocks/
type Decider interface {
	Decide(proposal types.Proposal, signatures []types.Signature)
}

//go:generate mockery -dir . -name FailureDetector -case underscore -output ./mocks/
type FailureDetector interface {
	Complain()
}

//go:generate mockery -dir . -name Synchronizer -case underscore -output ./mocks/
type Synchronizer interface {
	SyncIfNeeded()
}

//go:generate mockery -dir . -name Comm -case underscore -output ./mocks/
type Comm interface {
	Broadcast(m *protos.Message)
}

type Logger interface {
	Debugf(template string, args ...interface{})
	Infof(template string, args ...interface{})
	Errorf(template string, args ...interface{})
	Warnf(template string, args ...interface{})
	Panicf(template string, args ...interface{})
}

//go:generate mockery -dir . -name Signer -case underscore -output ./mocks/
type Signer interface {
	Sign([]byte) []byte
	SignProposal(types.Proposal) *types.Signature
}

//go:generate mockery -dir . -name Verifier -case underscore -output ./mocks/
type Verifier interface {
	VerifyProposal(proposal types.Proposal, prevHeader []byte) error
	VerifyRequest(val []byte) error
	VerifyConsenterSig(signer uint64, signature []byte, prop types.Proposal) error
	VerificationSequence() uint64
}

type View struct {
	// Configuration
	N                int
	LeaderID         uint64
	Number           uint64
	Decider          Decider
	FailureDetector  FailureDetector
	Sync             Synchronizer
	Logger           Logger
	Comm             Comm
	Verifier         Verifier
	Signer           Signer
	ProposalSequence uint64
	PrevHeader       []byte
	// Runtime
	incMsgs   chan *incMsg
	proposals chan types.Proposal // size of 1
	// Current proposal
	prepares *voteSet
	commits  *voteSet
	// Next proposal
	nextPrepares *voteSet
	nextCommits  *voteSet

	abortChan chan struct{}

	lock sync.RWMutex // a lock on the sequence and all votes
}

func (v *View) Start() Future {
	v.incMsgs = make(chan *incMsg, 10*v.N) // TODO channel size should be configured
	v.proposals = make(chan types.Proposal, 1)
	v.abortChan = make(chan struct{})

	var viewEnds sync.WaitGroup
	viewEnds.Add(2)

	go func() {
		defer viewEnds.Done()
		v.processMessages()
	}()
	go func() {
		defer viewEnds.Done()
		v.run()
	}()

	return &viewEnds
}

func (v *View) Sequence() uint64 {
	v.lock.RLock()
	defer v.lock.RUnlock()
	return v.ProposalSequence
}

func (v *View) processMessages() {
	v.setupVotes()

	for {
		select {
		case <-v.abortChan:
			return
		case msg := <-v.incMsgs:
			v.processMsg(msg.sender, msg.Message)
		}
	}
}

func (v *View) setupVotes() {
	// Prepares
	acceptPrepares := func(_ uint64, message *protos.Message) bool {
		return message.GetPrepare() != nil
	}

	v.prepares = &voteSet{
		validVote: acceptPrepares,
	}
	v.prepares.clear(v.N)

	v.nextPrepares = &voteSet{
		validVote: acceptPrepares,
	}
	v.nextPrepares.clear(v.N)

	// Commits
	acceptCommits := func(sender uint64, message *protos.Message) bool {
		commit := message.GetCommit()
		if commit == nil {
			return false
		}
		if commit.Signature == nil {
			return false
		}
		// Sender needs to match the inner signature sender
		return commit.Signature.Signer == sender
	}

	v.commits = &voteSet{
		validVote: acceptCommits,
	}
	v.commits.clear(v.N)

	v.nextCommits = &voteSet{
		validVote: acceptCommits,
	}
	v.nextCommits.clear(v.N)
}

func (v *View) HandleMessage(sender uint64, m *protos.Message) {
	msg := &incMsg{sender: sender, Message: m}
	select {
	case <-v.abortChan:
		return
	case v.incMsgs <- msg:
	}
}

func (v *View) processMsg(sender uint64, m *protos.Message) {
	v.Logger.Debugf("Got message %v from %d", m, sender)
	// Ensure view number is equal to our view
	msgViewNum := viewNumber(m)
	if msgViewNum != v.Number {
		v.Logger.Warnf("Got message %v from %d of view %d, expected view %d", m, sender, msgViewNum, v.Number)
		// TODO  when do we send the error message?
		if sender != v.LeaderID {
			return
		}
		// Else, we got a message with a wrong view from the leader.
		v.FailureDetector.Complain()
		v.Sync.SyncIfNeeded()
		v.Abort()
		return
	}

	// TODO: what if proposal sequence is wrong?
	// we should handle it...
	// but if we got a prepare or commit for sequence i,
	// when we're in sequence i+1 then we should still send a prepare/commit again.
	// leaving this as a task for now.

	v.lock.RLock()

	msgProposalSeq := proposalSequence(m)

	// This message is either for this proposal or the next one (we might be behind the rest)
	if msgProposalSeq != v.ProposalSequence && msgProposalSeq != v.ProposalSequence-1 && msgProposalSeq != v.ProposalSequence+1 {
		v.Logger.Warnf("Got message from %d with sequence %d but our sequence is %d", sender, msgProposalSeq, v.ProposalSequence)
		return
	}

	msgForNextProposal := msgProposalSeq == v.ProposalSequence+1

	if pp := m.GetPrePrepare(); pp != nil {
		v.lock.RUnlock()
		v.handlePrePrepare(sender, pp)
		return
	}

	defer v.lock.RUnlock()

	if prp := m.GetPrepare(); prp != nil {
		if msgForNextProposal {
			v.nextPrepares.registerVote(sender, m)
		} else {
			v.prepares.registerVote(sender, m)
		}
		return
	}

	if cmt := m.GetCommit(); cmt != nil {
		if msgForNextProposal {
			v.nextCommits.registerVote(sender, m)
		} else {
			v.commits.registerVote(sender, m)
		}
		return
	}
}

func (v *View) handlePrePrepare(sender uint64, pp *protos.PrePrepare) {
	if pp.Proposal == nil {
		v.Logger.Warnf("Got pre-prepare with empty proposal")
		return
	}
	if sender != v.LeaderID {
		v.Logger.Warnf("Got pre-prepare from %d but the leader is %d", sender, v.LeaderID)
		return
	}

	prop := pp.Proposal
	proposal := types.Proposal{
		VerificationSequence: int64(prop.VerificationSequence),
		Metadata:             prop.Metadata,
		Payload:              prop.Payload,
		Header:               prop.Header,
	}
	select {
	case v.proposals <- proposal:
	default:
		// A proposal is currently being handled.
		// Log a warning because we shouldn't get 2 proposals from the leader within such a short time.
		// We can have an outstanding proposal in the channel, but not more than 1.
		v.Logger.Warnf("Got proposal %d but currently still not processed proposal %d", pp.Seq, v.ProposalSequence)
	}
}

func (v *View) run() {
	for {
		select {
		case <-v.abortChan:
			return
		default:
			v.doStep()
			v.startNextSeq()
		}
	}
}

func (v *View) doStep() {
	proposal := v.processProposal()
	if proposal == nil {
		// Aborted view
		return
	}

	v.processPrepares(proposal)

	signatures := v.processCommits(proposal)
	if len(signatures) == 0 {
		return
	}

	v.maybeDecide(proposal, signatures)
}

func (v *View) processProposal() *types.Proposal {
	var proposal types.Proposal
	select {
	case <-v.abortChan:
		return nil
	case proposal = <-v.proposals:
	}
	// TODO think if there is any other validation the node should run on a proposal
	err := v.Verifier.VerifyProposal(proposal, v.PrevHeader)
	if err != nil {
		v.Logger.Warnf("Received bad proposal from %d: %v", v.LeaderID, err)
		v.FailureDetector.Complain()
		v.Sync.SyncIfNeeded()
		v.Abort()
		return nil
	}

	v.lock.RLock()
	seq := v.ProposalSequence
	v.lock.RUnlock()

	msg := &protos.Message{
		Content: &protos.Message_Prepare{
			Prepare: &protos.Prepare{
				Seq:    seq,
				View:   v.Number,
				Digest: proposal.Digest(),
			},
		},
	}

	v.Comm.Broadcast(msg)
	return &proposal
}

func (v *View) processPrepares(proposal *types.Proposal) {
	expectedDigest := proposal.Digest()
	collectedDigests := 0
	quorum := v.quorum()

	for collectedDigests < quorum-1 {
		select {
		case <-v.abortChan:
			return
		case vote := <-v.prepares.votes:
			prepare := vote.GetPrepare()
			if prepare.Digest != expectedDigest {
				// TODO is it ok that the vote is already registered?
				v.Logger.Warnf("Got digest %s but expected %s", prepare.Digest, expectedDigest)
				continue
			}
			collectedDigests++
		}
	}

	sig := v.Signer.SignProposal(*proposal)

	v.lock.RLock()
	seq := v.ProposalSequence
	v.lock.RUnlock()

	msg := &protos.Message{
		Content: &protos.Message_Commit{
			Commit: &protos.Commit{
				View:   v.Number,
				Digest: expectedDigest,
				Seq:    seq,
				Signature: &protos.Signature{
					Signer: sig.Id,
					Value:  sig.Value,
				},
			},
		},
	}

	v.Comm.Broadcast(msg)
}

func (v *View) processCommits(proposal *types.Proposal) []types.Signature {
	expectedDigest := proposal.Digest()
	signatures := make(map[uint64]types.Signature)
	quorum := v.quorum()

	for len(signatures) < quorum-1 {
		select {
		case <-v.abortChan:
			return nil
		case vote := <-v.commits.votes:
			commit := vote.GetCommit()
			if commit.Digest != expectedDigest {
				v.Logger.Warnf("Got digest %s but expected %s", commit.Digest, expectedDigest)
				continue
			}

			err := v.Verifier.VerifyConsenterSig(commit.Signature.Signer, commit.Signature.Value, *proposal)
			if err != nil {
				v.Logger.Warnf("Couldn't verify %d's signature: %v", commit.Signature.Signer, err)
				continue
			}

			signatures[commit.Signature.Signer] = types.Signature{
				Id:    commit.Signature.Signer,
				Value: commit.Signature.Value,
			}
		}
	}

	var res []types.Signature
	for _, sig := range signatures {
		res = append(res, sig)
	}
	return res
}

func (v *View) maybeDecide(proposal *types.Proposal, signatures []types.Signature) {
	mySig := v.Signer.SignProposal(*proposal)
	signatures = append(signatures, *mySig)
	v.Decider.Decide(*proposal, signatures)
	v.lock.RLock()
	seq := v.ProposalSequence
	v.lock.RUnlock()
	v.Logger.Infof("Decided on %d", seq)
}

func (v *View) startNextSeq() {
	v.lock.Lock()
	defer v.lock.Unlock()

	prevSeq := v.ProposalSequence

	v.ProposalSequence++

	nextSeq := v.ProposalSequence

	v.Logger.Infof("Sequence: %d-->%d", prevSeq, nextSeq)

	// swap next prepares
	tmpVotes := v.prepares
	v.prepares = v.nextPrepares
	tmpVotes.clear(v.N)
	v.nextPrepares = tmpVotes

	// swap next commits
	tmpVotes = v.commits
	v.commits = v.nextCommits
	tmpVotes.clear(v.N)
	v.nextCommits = tmpVotes
}

func (v *View) Propose() {

}

func (v *View) Abort() {
	select {
	case <-v.abortChan:
		// already aborted
		return
	default:
		close(v.abortChan)
	}
}

func (v *View) quorum() int {
	f := int(math.Floor((float64(v.N) - 1.0) / 3.0))
	q := int(math.Ceil((float64(v.N) + float64(f) + 1) / 2.0))
	//v.Logger.Debugf("The number of nodes (N) is %d, F is %d, and the quorum size is %d", v.N, f, q)
	return q
}

type voteSet struct {
	validVote func(voter uint64, message *protos.Message) bool
	voted     map[uint64]struct{}
	votes     chan *protos.Message
}

func (vs *voteSet) clear(n int) {
	// Drain the votes channel
	for len(vs.votes) > 0 {
		<-vs.votes
	}

	vs.voted = make(map[uint64]struct{}, n)
	vs.votes = make(chan *protos.Message, n)
}

func (vs *voteSet) registerVote(voter uint64, message *protos.Message) {
	if !vs.validVote(voter, message) {
		return
	}

	_, hasVoted := vs.voted[voter]
	if hasVoted {
		// Received double vote
		return
	}

	vs.voted[voter] = struct{}{}
	vs.votes <- message
}

type incMsg struct {
	*protos.Message
	sender uint64
}
